package main

import (
	"fmt"
	"log"
	"os"
	"strconv"

	"github.com/eventials/go-tus"
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
		le.Log(ale.Critical, fmt.Sprintf("Failed to get API Token from Transporter Pattern. Err: %s", acctIDErr))
		os.Exit(1)
	}

	// Chunk Size for TUS Upload
	chunkSizeStr, chunkSizeStrErr := pattern.Get("chunksize")
	if chunkSizeStrErr != nil {
		le.Log(ale.Critical, fmt.Sprintf("Failed to get Chunk Size from Transporter Pattern. Err: %s", acctIDErr))
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
	fileSize := fileInfo.Size()
	f, err := os.Open(file)
	if err != nil {
		le.Log(ale.Critical, fmt.Sprintf("Failed to open file for upload. Err: %s", err))
		os.Exit(1)
	}
	defer f.Close()
	//#endregion Transporter / Inputs / Parsing

	screen, err0 := tcell.NewScreen()
	if err0 != nil {
		fmt.Print(err0)
	}

	err1 := screen.Init()
	if err1 != nil {
		fmt.Print(err1)
	}

	screenW, _ := screen.Size()

	config := &tus.Config{
		ChunkSize:           chunkSize * 1024 * 1024,
		Resume:              false,
		OverridePatchMethod: false,
		Store:               nil,
		Header: map[string][]string{
			"Authorization": {fmt.Sprintf("Bearer %s", apiToken)},
		},
		HttpClient: nil,
	}

	clientURL := fmt.Sprintf(endpoint, accountID)
	fmt.Println(clientURL)
	client, err := tus.NewClient(clientURL, config)
	if err != nil {
		log.Fatalf("Failed to create TUS client: %v", err)
	}

	upload, err := tus.NewUploadFromFile(f)
	if err != nil {
		log.Fatalf("Failed to create upload from file: %v", err)
	}

	uploader, err := client.CreateUpload(upload)
	if err != nil {
		log.Fatalf("Failed to create upload: %v", err)
	}

	go func() {
		for {
			ev := screen.PollEvent()
			switch ev := ev.(type) {
			case *tcell.EventKey:
				modifier, key, _ := ev.Modifiers(), ev.Key(), ev.Rune()
				//logEngine.Log(logEngine.CreateLogNow(ale.Debug, "",fmt.Sprintf("Mods: %f| Key: %f| Rune: %f", modifier, key, char)))
				if modifier == 2 && key == 3 {
					// Control C | Terminate
					screen.Clear()
					screen.Sync()
					screen.Fini()
					//logEngine.Log(logEngine.CreateLogNow(ale.Debug, "","Detected Ctrl-C. Exiting TC..."))
					os.Exit(1)
				}
			}
		}
	}()

	x := 0
	for {
		// Take Event or Render / Display

		// Clear screen
		// screen.Clear()
		x += 1
		tCellDraw(screen, 15, 11, 20, 11, tcell.StyleDefault, fmt.Sprint(x))
		// Boundaries
		tCellDraw(screen, 0, 1, screenW, 1, tcell.StyleDefault, getChars("~", screenW))

		// Text
		line := fmt.Sprintf("Account ID: %s", accountID)
		tCellDraw(screen, 0, 0, len(line), 0, tcell.StyleDefault, line)

		// Progress
		line = fmt.Sprintf("\t ==> File: %s", f.Name())
		tCellDraw(screen, 0, 3, len(line), 3, tcell.StyleDefault, line)

		offset := uploader.Offset()
		line = fmt.Sprintf("\t\t || Bytes Uploaded? Offset: %d", offset)
		tCellDraw(screen, 0, 4, len(line), 4, tcell.StyleDefault, line)
		line = fmt.Sprintf("\t\t || Total File Size: %d", fileSize)
		tCellDraw(screen, 0, 5, len(line), 5, tcell.StyleDefault, line)

		err := uploader.UploadChunck()
		line = fmt.Sprintf("Errors: %s", err)
		tCellDraw(screen, 0, 12, len(line), 12, tcell.StyleDefault, line)

		// Display
		screen.Show()

		//time.Sleep(time.Millisecond * 16)

	}
}
