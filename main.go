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
	pCTX, _ := pconsole.New(20, 20)
	le.AddLogPipeline(ale.Info, pCTX.Log)

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
				},
			},
		}, transporter.TransporterOptions{
			EnviormentPrefix:         "T_",
			DumpEnvironmentVariables: false,
			DumpCLIArguments:         false,
			LogEngine:                le,
			LogEnginePConsoleCTX:     pCTX,
			ConfigFileEngine:         nil,
		},
	)
	if err2 != nil {
		fmt.Print(err2)
	}

	// Get details from Transporter like Account ID and API Token
	accountID, acctIDErr := pattern.Get("acctid")
	if acctIDErr != nil {
		fmt.Print(acctIDErr)
	}
	apiToken, apiTokenIDErr := pattern.Get("apitoken")
	if apiTokenIDErr != nil {
		fmt.Print(apiTokenIDErr)
	}
	chunkSizeStr, chunkSizeStrErr := pattern.Get("chunksize")
	if chunkSizeStrErr != nil {
		fmt.Print(chunkSizeStrErr)
	}
	chunkSizeInt, chunkSizeConvErr := strconv.Atoi(chunkSizeStr)

	if chunkSizeConvErr != nil {
		fmt.Print(chunkSizeConvErr)
	}
	var chunkSize int64 = int64(chunkSizeInt)

	// Get File Details and Handle
	file, fileErr := pattern.Get("file")
	if fileErr != nil {
		fmt.Print(fileErr)
	}
	fileInfo, err := os.Stat(file)
	if err != nil {
		fmt.Println("Error:", err)
		return
	}
	fileSize := fileInfo.Size()
	f, err := os.Open(file)
	if err != nil {
		log.Fatalf("Failed to open file: %v", err)
	}
	defer f.Close()

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
		ChunkSize:           chunkSize * 1024 * 1024, // Adjust chunk size as needed
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
