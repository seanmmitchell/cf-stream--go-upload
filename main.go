package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"syscall"

	"github.com/eventials/go-tus"
	"github.com/eventials/go-tus/memorystore"
	"github.com/gdamore/tcell/v2"
	"github.com/seanmmitchell/ale/v2"
	"github.com/seanmmitchell/ale/v2/pconsole"
	"github.com/seanmmitchell/transporter"
)

const (
	endpoint = "https://api.cloudflare.com/client/v4/accounts/%s/stream"
)

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
	}

	// Get details from Transporter like Account ID and API Token
	accountID, acctIDErr := pattern.Get("acctid")
	if acctIDErr != nil {
		le.Log(ale.Critical, fmt.Sprintf("Failed to get Account ID from Transporter Pattern. Err: %s", acctIDErr))
		os.Exit(1)
	}
	apiToken, apiTokenIDErr := pattern.Get("apitoken")
	if apiTokenIDErr != nil {
		le.Log(ale.Critical, fmt.Sprintf("Failed to get API Token from Transporter Pattern. Err: %s", apiTokenIDErr))
		os.Exit(1)
	}

	// A token passed as a flag is visible in ps and shell history for as long as the upload runs.
	for _, arg := range os.Args[1:] {
		if arg == "--token" || arg == "--apitoken" {
			le.Log(ale.Warning, "The API token was passed on the command line, where it shows up in ps and shell history. Set the T_apitoken environment variable instead.")
			break
		}
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
	fileInfo, err := os.Stat(file)
	if err != nil {
		le.Log(ale.Critical, fmt.Sprintf("Failed to get file details. Err: %s", err))
		os.Exit(1)
		return
	}
	// Not !IsRegular(): on Windows that also rejects OneDrive and other reparse-point files.
	if fileInfo.Mode()&(os.ModeDir|os.ModeNamedPipe|os.ModeSocket|os.ModeDevice|os.ModeCharDevice) != 0 {
		le.Log(ale.Critical, fmt.Sprintf("File must be a regular file, not a directory, pipe, socket or device: %s", file))
		os.Exit(1)
	}
	fileSize := fileInfo.Size()
	f, err := os.Open(file)
	if err != nil {
		le.Log(ale.Critical, fmt.Sprintf("Failed to open file for upload. Err: %s", err))
		os.Exit(1)
	}
	defer f.Close()
	//#endregion Transporter / Inputs / Parsing

	//#region TUS Client
	// Resume keeps the upload URL in the store, so after a failed chunk the
	// uploader can re-read the server's offset (ResumeUpload) before resending.
	store, _ := memorystore.NewMemoryStore()
	// ctx is the upload worker's; cancelling it also aborts in-flight requests.
	ctx, cancel := context.WithCancel(context.Background())
	config := &tus.Config{
		ChunkSize:           chunkSize * 1024 * 1024,
		Resume:              true,
		OverridePatchMethod: false,
		Store:               store,
		Header: map[string][]string{
			"Authorization": {fmt.Sprintf("Bearer %s", apiToken)},
		},
		HttpClient: newHTTPClient(ctx),
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
	//#endregion TUS Client

	// Catch the usual stop signals before the terminal goes raw, so they can't
	// skip Fini. SIGQUIT keeps Go's default so it can still dump goroutines.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)

	// Set up the screen before the upload is created on the server, so a
	// missing or unusable terminal stops here without leaving a partial upload.
	screen, err := tcell.NewScreen()
	if err != nil {
		le.Log(ale.Critical, fmt.Sprintf("Failed to open the terminal screen. Err: %s", err))
		os.Exit(1)
	}
	if err := screen.Init(); err != nil {
		le.Log(ale.Critical, fmt.Sprintf("Failed to initialize the terminal screen. Err: %s", err))
		os.Exit(1)
	}

	// The worker owns the upload and only reports progress. runUI owns the
	// screen and restores the terminal before it returns.
	updates := make(chan progress)
	done := make(chan error, 1)
	retry := defaultRetry
	go func() {
		// A panic here would skip runUI's Fini, so hand it to runUI as an error.
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("upload panicked: %v\n%s", r, debug.Stack())
			}
		}()
		done <- uploadFile(ctx, client, upload, retry, func(p progress) {
			select {
			case updates <- p:
			case <-ctx.Done():
			}
		})
	}()

	v := view{accountID: accountID, fileName: f.Name(), fileSize: fileSize, maxRetries: retry.maxRetries}
	last, err := runUI(screen, v, updates, done, signals)
	cancel()

	// Name the partial upload, so it can be found and deleted in the dashboard.
	partial := ""
	if last.url != "" {
		partial = fmt.Sprintf(" Partial upload: %s", last.url)
	}
	var stop stopSignal
	switch {
	case err == nil:
		le.Log(ale.Info, fmt.Sprintf("Upload complete. %d bytes sent to %s", last.offset, last.url))
	case errors.Is(err, errInterrupted):
		le.Log(ale.Warning, fmt.Sprintf("Upload cancelled after %d of %d bytes.%s", last.offset, fileSize, partial))
		os.Exit(130)
	case errors.As(err, &stop):
		le.Log(ale.Warning, fmt.Sprintf("Upload stopped by signal (%s) after %d of %d bytes.%s", stop.sig, last.offset, fileSize, partial))
		os.Exit(128 + int(stop.sig))
	default:
		le.Log(ale.Critical, fmt.Sprintf("Upload failed after %d of %d bytes.%s Err: %s", last.offset, fileSize, partial, describe(err)))
		os.Exit(1)
	}
}
