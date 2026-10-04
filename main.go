package main

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/eventials/go-tus"
	"github.com/eventials/go-tus/memorystore"
	"github.com/gdamore/tcell/v2"
	"github.com/seanmmitchell/ale/v2"
	"github.com/seanmmitchell/ale/v2/pconsole"
	"github.com/seanmmitchell/transporter"
)

const (
	endpoint = "https://api.cloudflare.com/client/v4/accounts/%s/stream"

	// Transient chunk failures (network errors, timeouts, 5xx, 408, 429) are retried with an
	// exponential backoff. The upload is abandoned after this many consecutive failures.
	maxAttempts    = 8
	retryBaseDelay = 1 * time.Second
	retryMaxDelay  = 30 * time.Second

	// How long to wait for response headers once a request, including its chunk, has been sent.
	responseTimeout = 90 * time.Second

	// Longest Cloudflare error body that is shown.
	maxErrorBody = 2048

	// Exit code when the upload is canceled with Ctrl-C (128 + SIGINT).
	exitCanceled = 130
)

// uploadStatus is sent by the upload worker to the main goroutine, which owns the screen.
type uploadStatus struct {
	state   string    // What the worker is doing.
	offset  int64     // Bytes the server has confirmed.
	url     string    // Upload URL, once the upload has been created.
	lastErr error     // Latest failure being retried. Cleared after a successful chunk.
	attempt int       // Consecutive failed attempts.
	retryAt time.Time // When the next attempt starts.
	done    bool      // The whole file was uploaded.
	err     error     // The upload was abandoned.

	panicValue any // A panic recovered in the worker, re-raised by the main goroutine after Fini.
	panicStack []byte
}

func main() {
	le := ale.CreateLogEngine("Cloudflare Stream - Go Uploader")
	pCTX, _ := pconsole.New(50, 20)
	le.AddLogPipeline(ale.Info, pCTX.Log)

	tle := le.CreateSubEngine("Transporter")
	tle.AddLogPipeline(ale.Info, pCTX.Log)

	//#region Transporter / Inputs / Parsing
	pattern, err2 := transporter.Energize(
		transporter.Pattern{
			Sequences: map[string]transporter.PatternSequence{
				"acctid": {
					Name:        "Account ID",
					Description: "",
					CLIFlags:    []string{"acctid"},
					ENVVars:     []string{"acctid"},
				},
				"apitoken": {
					Name:               "API Token",
					Description:        "",
					CLIFlags:           []string{"apitoken", "token"},
					ENVVars:            []string{"apitoken"},
					DisablePersistence: true,
				},
				"file": {
					Name:               "File",
					Description:        "",
					CLIFlags:           []string{"file"},
					ENVVars:            []string{"file"},
					DisablePersistence: true,
				},
				"chunksize": {
					Name:               "Chunk Size",
					Description:        "",
					CLIFlags:           []string{"chunksize"},
					ENVVars:            []string{"chunksize"},
					DisablePersistence: true,
					Value:              "5",
				},
			},
		}, transporter.TransporterOptions{
			EnviormentPrefix:         "T_",
			DumpEnvironmentVariables: false,
			DumpCLIArguments:         false,
			LogEngine:                tle,
			LogEnginePConsoleCTX:     pCTX,
			ConfigFileEngine:         nil,
		},
	)
	if err2 != nil {
		le.Log(ale.Critical, fmt.Sprintf("Transporter pattern failed to energize. Err: %s", err2))
		os.Exit(1)
	}

	// Get details from Transporter like Account ID and API Token. Unset values come back empty.
	accountID, acctIDErr := pattern.Get("acctid")
	if acctIDErr != nil {
		le.Log(ale.Critical, fmt.Sprintf("Failed to get Account ID from Transporter Pattern. Err: %s", acctIDErr))
		os.Exit(1)
	}
	if strings.TrimSpace(accountID) == "" {
		le.Log(ale.Critical, "No Account ID was provided. Use --acctid or the T_acctid environment variable.")
		os.Exit(1)
	}
	apiToken, apiTokenErr := pattern.Get("apitoken")
	if apiTokenErr != nil {
		le.Log(ale.Critical, fmt.Sprintf("Failed to get API Token from Transporter Pattern. Err: %s", apiTokenErr))
		os.Exit(1)
	}
	if strings.TrimSpace(apiToken) == "" {
		le.Log(ale.Critical, "No API Token was provided. Set the T_apitoken (or T_token) environment variable, or use --apitoken/--token.")
		os.Exit(1)
	}

	// Chunk Size for TUS Upload
	chunkSizeStr, chunkSizeStrErr := pattern.Get("chunksize")
	if chunkSizeStrErr != nil {
		le.Log(ale.Critical, fmt.Sprintf("Failed to get Chunk Size from Transporter Pattern. Err: %s", chunkSizeStrErr))
		os.Exit(1)
	}
	chunkSizeInt, chunkSizeConvErr := strconv.Atoi(chunkSizeStr)
	if chunkSizeConvErr != nil {
		le.Log(ale.Critical, fmt.Sprintf("Failed to get Chunk Size from Transporter Pattern. Err: %s", chunkSizeConvErr))
		os.Exit(1)
	}

	// Confirm Chunk Size in CF Stream bounds.
	if chunkSizeInt < 5 || chunkSizeInt > 200 {
		le.Log(ale.Error, "An invalid chunk size was provided. Please select a value between 5-200.")
		os.Exit(1)
	}

	var chunkSize int64 = int64(chunkSizeInt)

	// Get File Details and Handle
	file, fileErr := pattern.Get("file")
	if fileErr != nil {
		le.Log(ale.Critical, fmt.Sprintf("Failed to get File from Transporter Pattern. Err: %s", fileErr))
		os.Exit(1)
	}
	if strings.TrimSpace(file) == "" {
		le.Log(ale.Critical, "No File was provided. Use --file or the T_file environment variable.")
		os.Exit(1)
	}
	// Checked before opening so a FIFO or device is rejected instead of blocking in Open.
	pathInfo, err := os.Stat(file)
	if err != nil {
		le.Log(ale.Critical, fmt.Sprintf("Failed to get file details. Err: %s", err))
		os.Exit(1)
	}
	if !pathInfo.Mode().IsRegular() {
		le.Log(ale.Critical, fmt.Sprintf("\"%s\" is not a regular file.", file))
		os.Exit(1)
	}
	f, err := os.Open(file)
	if err != nil {
		le.Log(ale.Critical, fmt.Sprintf("Failed to open file for upload. Err: %s", err))
		os.Exit(1)
	}
	defer f.Close()
	fileInfo, err := f.Stat()
	if err != nil {
		le.Log(ale.Critical, fmt.Sprintf("Failed to get file details. Err: %s", err))
		os.Exit(1)
	}
	if !fileInfo.Mode().IsRegular() {
		le.Log(ale.Critical, fmt.Sprintf("\"%s\" is not a regular file.", file))
		os.Exit(1)
	}
	if fileInfo.Size() == 0 {
		le.Log(ale.Critical, fmt.Sprintf("\"%s\" is empty.", file))
		os.Exit(1)
	}
	//#endregion Transporter / Inputs / Parsing

	// Resume + Store let a new uploader pick up at the server's offset (HEAD) after a failed
	// chunk, e.g. when the server stored it but the response was lost.
	store, _ := memorystore.NewMemoryStore()
	// The default transport's proxy settings and dial / TLS handshake timeouts, plus a limit on
	// waiting for a response. No overall Client.Timeout: a 200 MB chunk can take a long time to
	// send on a slow link.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = responseTimeout

	config := &tus.Config{
		ChunkSize:           chunkSize * 1024 * 1024,
		Resume:              true,
		OverridePatchMethod: false,
		Store:               store,
		Header: map[string][]string{
			"Authorization": {fmt.Sprintf("Bearer %s", apiToken)},
		},
		HttpClient: &http.Client{Transport: transport},
	}

	client, err := tus.NewClient(fmt.Sprintf(endpoint, accountID), config)
	if err != nil {
		le.Log(ale.Critical, fmt.Sprintf("Failed to create TUS client. Err: %s", err))
		os.Exit(1)
	}

	upload, err := tus.NewUploadFromFile(f)
	if err != nil {
		le.Log(ale.Critical, fmt.Sprintf("Failed to create upload from file. Err: %s", err))
		os.Exit(1)
	}
	fileSize := upload.Size()

	// Registered before the screen is set up so a signal can't skip restoring the terminal.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)

	// Set up the screen before any request, so a missing terminal doesn't leave a half-made upload.
	screen, err := tcell.NewScreen()
	if err == nil {
		err = screen.Init()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to start the terminal UI; run cfsgo in an interactive terminal (with TERM set). Err: %s\n", err)
		os.Exit(1)
	}

	// The terminal is now in raw mode on the alternate screen. Every exit from here on goes
	// through quit, which restores it first.
	quit := func(code int, out *os.File, msg string) {
		screen.Fini()
		fmt.Fprintln(out, msg)
		os.Exit(code)
	}
	defer func() {
		if r := recover(); r != nil {
			screen.Fini()
			panic(r)
		}
	}()

	// tcell is not safe to draw to or Fini from several goroutines, so only this goroutine
	// touches the screen. Events and upload progress are passed to it over channels.
	events := make(chan tcell.Event)
	go func() {
		for {
			ev := screen.PollEvent()
			if ev == nil {
				// Screen finalized.
				return
			}
			events <- ev
		}
	}()

	updates := make(chan uploadStatus, 8)
	go uploadWorker(client, upload, updates)

	status := uploadStatus{state: "Creating upload"}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		draw(screen, accountID, f.Name(), fileSize, chunkSize, status)

		select {
		case ev := <-events:
			switch ev := ev.(type) {
			case *tcell.EventKey:
				// Raw mode turns Ctrl-C into a key press instead of SIGINT.
				if ev.Key() == tcell.KeyCtrlC {
					// Control C | Terminate
					quit(exitCanceled, os.Stderr, fmt.Sprintf("Upload canceled. %d of %d bytes were uploaded.", status.offset, fileSize))
				}
			case *tcell.EventResize:
				screen.Sync()
			}
		case sig := <-signals:
			code := 1
			if s, ok := sig.(syscall.Signal); ok {
				code = 128 + int(s)
			}
			quit(code, os.Stderr, fmt.Sprintf("Upload canceled (%s). %d of %d bytes were uploaded.", sig, status.offset, fileSize))
		case st := <-updates:
			if st.panicValue != nil {
				screen.Fini()
				fmt.Fprintf(os.Stderr, "Upload worker panicked:\n%s\n", st.panicStack)
				panic(st.panicValue)
			}
			status = st
			if st.err != nil {
				msg := "Upload failed: " + describeError(st.err)
				if st.url != "" {
					msg += fmt.Sprintf("\nVideo ID: %s (incomplete, %d of %d bytes uploaded)", videoID(st.url), st.offset, fileSize)
				}
				quit(1, os.Stderr, msg)
			}
			if st.done {
				quit(0, os.Stdout, fmt.Sprintf("Upload complete.\n  File: %s\n  Bytes Uploaded: %d\n  Video ID: %s\n  Upload URL: %s",
					f.Name(), st.offset, videoID(st.url), st.url))
			}
		case <-ticker.C:
			// Redraw, e.g. to count down to the next retry.
		}
	}
}

// uploadWorker creates the upload and sends the file chunk by chunk, reporting on updates.
// It never touches the screen and stops after sending a status with done or err set.
func uploadWorker(client *tus.Client, upload *tus.Upload, updates chan<- uploadStatus) {
	defer func() {
		if r := recover(); r != nil {
			updates <- uploadStatus{panicValue: r, panicStack: debug.Stack()}
		}
	}()

	st := uploadStatus{state: "Creating upload"}
	updates <- st

	// Not retried: if the response was lost the upload may already exist, and a retry would
	// create a second video.
	uploader, err := client.CreateUpload(upload)
	if err != nil {
		st.err = fmt.Errorf("creating the upload: %w", err)
		updates <- st
		return
	}
	st.url = uploader.Url()

	resync := false
	for {
		err = nil
		if resync {
			st.state = "Checking the upload offset with the server"
			updates <- st
			var resumed *tus.Uploader
			if resumed, err = client.ResumeUpload(upload); err == nil {
				uploader, resync = resumed, false
			}
		}

		if err == nil {
			st.offset = uploader.Offset()
			if st.offset >= upload.Size() {
				st.state, st.done = "Done", true
				updates <- st
				return
			}
			st.state = "Uploading"
			updates <- st
			if err = uploader.UploadChunck(); err == nil {
				st.lastErr, st.attempt = nil, 0
				continue
			}
		}

		if !retryable(err) {
			st.err = fmt.Errorf("uploading at offset %d: %w", st.offset, err)
			updates <- st
			return
		}
		st.lastErr = err
		st.attempt++
		if st.attempt >= maxAttempts {
			st.err = fmt.Errorf("giving up after %d failed attempts at offset %d: %w", st.attempt, st.offset, err)
			updates <- st
			return
		}
		delay := retryDelay(st.attempt)
		st.state, st.retryAt = "Retrying", time.Now().Add(delay)
		updates <- st
		time.Sleep(delay)

		// The server may have stored more (or less) than we think, so re-read its offset first.
		resync = true
	}
}

// retryable reports whether a failed request is worth retrying: network errors and timeouts,
// 408, 429 and 5xx responses, and offset mismatches (fixed by re-reading the server's offset).
// Other responses, protocol errors and local file errors are fatal.
func retryable(err error) bool {
	var clientErr tus.ClientError
	var urlErr *url.Error
	switch {
	case errors.Is(err, tus.ErrOffsetMismatch):
		return true
	case errors.As(err, &clientErr):
		return clientErr.Code >= 500 || clientErr.Code == http.StatusRequestTimeout || clientErr.Code == http.StatusTooManyRequests
	case errors.As(err, &urlErr):
		// http.Client wraps every transport failure (including timeouts) in a *url.Error.
		return true
	}
	return false
}

// retryDelay returns the wait before the next attempt after attempt consecutive failures:
// 1s, 2s, 4s, ... capped at retryMaxDelay, plus up to 20% jitter.
func retryDelay(attempt int) time.Duration {
	delay := retryBaseDelay
	for i := 1; i < attempt && delay < retryMaxDelay; i++ {
		delay *= 2
	}
	delay = min(delay, retryMaxDelay)
	return delay + rand.N(delay/5)
}

// describeError returns the error message followed by Cloudflare's response body, if any.
func describeError(err error) string {
	msg := err.Error()
	var clientErr tus.ClientError
	if errors.As(err, &clientErr) {
		body := clientErr.Body
		truncated := len(body) > maxErrorBody
		if truncated {
			body = body[:maxErrorBody]
		}
		// Drop control characters so the body can't send escape sequences to the terminal.
		text := strings.Map(func(r rune) rune {
			if unicode.IsControl(r) && r != '\n' && r != '\t' {
				return -1
			}
			return r
		}, strings.ToValidUTF8(string(body), ""))
		text = strings.TrimSpace(text)
		if truncated {
			text += " ...(truncated)"
		}
		if text != "" {
			msg += "\nCloudflare response: " + text
		}
	}
	return msg
}

// videoID returns the Stream video ID: the last path segment of the upload URL
// (e.g. https://upload.videodelivery.net/tus/<id>?tusv2=true).
func videoID(uploadURL string) string {
	u, err := url.Parse(uploadURL)
	if err != nil {
		return "unknown"
	}
	id := path.Base(u.Path)
	if id == "/" || id == "." {
		return "unknown"
	}
	return id
}

// draw renders the upload status. Only the main goroutine may call it.
func draw(screen tcell.Screen, accountID, file string, fileSize, chunkSize int64, st uploadStatus) {
	screenW, _ := screen.Size()
	style := tcell.StyleDefault

	// Start from a blank buffer so shorter lines don't leave stale text behind.
	screen.Clear()

	// Text
	tCellDraw(screen, 0, 0, style, fmt.Sprintf("Account ID: %s", accountID))
	// Boundaries
	tCellDraw(screen, 0, 1, style, getChars("~", screenW))

	// Progress
	tCellDraw(screen, 0, 3, style, fmt.Sprintf("  ==> File: %s", file))
	tCellDraw(screen, 0, 4, style, fmt.Sprintf("        || Bytes Uploaded: %d (%d%%)", st.offset, st.offset*100/max(fileSize, 1)))
	tCellDraw(screen, 0, 5, style, fmt.Sprintf("        || Total File Size: %d", fileSize))
	tCellDraw(screen, 0, 6, style, fmt.Sprintf("        || Chunk Size: %d MB", chunkSize))

	state := st.state
	if state == "Retrying" {
		// Rounded up, so the countdown never shows 0s while still waiting.
		wait := (max(time.Until(st.retryAt), 0) + time.Second - 1).Truncate(time.Second)
		state = fmt.Sprintf("Retrying in %s (failed attempts: %d of %d)", wait, st.attempt, maxAttempts)
	}
	tCellDraw(screen, 0, 7, style, fmt.Sprintf("        || Status: %s", state))
	if st.url != "" {
		tCellDraw(screen, 0, 8, style, fmt.Sprintf("        || Video ID: %s", videoID(st.url)))
	}

	errText := "none"
	if st.lastErr != nil {
		errText = strings.Join(strings.Fields(describeError(st.lastErr)), " ")
	}
	tCellDraw(screen, 0, 10, style, fmt.Sprintf("Errors: %s", errText))
	tCellDraw(screen, 0, 12, style, "Press Ctrl-C to cancel.")

	// Display
	screen.Show()
}
