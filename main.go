package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"
)

const (
	ProgramName = "github.com/keithmartin1982/downloader"
	Version     = "0.0.1"
	PlaceHolder = "http://127.0.0.1:8080/files/largefile.bin"
)

var (
	ALREADY_DOWNLOADED_ERROR = errors.New("already downloaded")
	progressChan             chan ProgressMessage
	downloading              bool
	currentSpeed             float64
	lastTime                 time.Time
	lastBytes                int64
)

func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%Downloader B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%7.1f %cB",
		float64(b)/float64(div), "KMGTPE"[exp])
}

func calculateBps(startTime time.Time, startBytes int64, endTime time.Time, endBytes int64) float64 {
	bytesTransferred := endBytes - startBytes
	duration := endTime.Sub(startTime)
	secondsElapsed := duration.Seconds()
	if secondsElapsed == 0 {
		return 0.0
	}
	bitsTransferred := float64(bytesTransferred) * 8
	bps := bitsTransferred / secondsElapsed
	return bps
}

func isValidURL(testUrl string) bool {
	u, err := url.Parse(testUrl)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return false
	}
	_, err = url.ParseRequestURI(testUrl)
	if err != nil {
		return false
	}
	return true
}

type ProgressReader struct {
	Reader     io.Reader
	Total      int64
	Downloaded int64
}

type ProgressMessage struct {
	Total      int64
	Downloaded int64
}

func (pr *ProgressReader) Read(p []byte) (int, error) {
	n, err := pr.Reader.Read(p)
	if n > 0 {
		pr.Downloaded += int64(n)
		progressChan <- ProgressMessage{
			Total:      pr.Total,
			Downloaded: pr.Downloaded,
		}
	}
	return n, err
}

type GUI struct {
	app        fyne.App
	Downloader *Downloader
}

func (g *GUI) start() {
	metadata := g.app.Metadata()
	window := g.app.NewWindow(fmt.Sprintf("%s %s build %d", metadata.Name, metadata.Version, metadata.Build))
	input := widget.NewEntry()
	input.SetPlaceHolder(PlaceHolder)
	// input.SetText(PlaceHolder) // test
	progressBar := widget.NewProgressBar()
	progressBar.Max = 100
	progressBar.Min = 0
	statusOutput := widget.NewLabelWithStyle("Waiting", fyne.TextAlignCenter, fyne.TextStyle{})
	progressText := widget.NewLabelWithStyle("Progress", fyne.TextAlignCenter, fyne.TextStyle{})
	content := container.NewVBox(
		input,
		widget.NewButton("Download", func() {
			if downloading {
				fmt.Println("Download already in progress")
				return
			}
			if isValidURL(input.Text) {
				g.Downloader.Addr = input.Text
				g.Downloader.FileSize = 0
				go func() {
					if err := g.Downloader.Download(); err != nil {
						downloading = false
						if errors.Is(ALREADY_DOWNLOADED_ERROR, err) {
							fyne.Do(func() {
								statusOutput.SetText(fmt.Sprintf("Already Downloaded:\n%s", g.Downloader.Addr))
							})
							g.Downloader.FileSize = -1
							downloading = true
							return
						}
						fyne.Do(func() {
							statusOutput.SetText(fmt.Sprintf("FAILED\n Download:\n%s", g.Downloader.Addr))
						})
						log.Printf("Download failed: %s", err)
					}
					downloading = false
				}()
				// Wait for download to start
				for !downloading {
					continue
				}
				if g.Downloader.FileSize == -1 {
					g.Downloader.FileSize = 0
					fmt.Println("Download cancelled")
					downloading = false
					return
				}
				fyne.Do(func() {
					statusOutput.SetText(fmt.Sprintf("Downloading:\n%s\nSize: %dMB\n", g.Downloader.Addr, g.Downloader.FileSize/1024/1024))
				})
				go func() {
					for {
						currentProgress := <-progressChan
						fyne.Do(func() {
							progressBar.SetValue(float64(currentProgress.Downloaded) / float64(currentProgress.Total) * 100)
							progressText.SetText(fmt.Sprintf("Downloaded %s of %s %7.2fMbps  ",
								formatBytes(currentProgress.Downloaded),
								formatBytes(currentProgress.Total),
								currentSpeed))
						})
						if currentProgress.Downloaded == currentProgress.Total && currentProgress.Downloaded > 0 {
							return
						}
					}
				}()
			} else {
				log.Println("Invalid Content:", input.Text)
			}
		}),
		progressBar,
		statusOutput,
		progressText,
	)
	window.SetContent(content)
	window.Resize(fyne.NewSize(500, 100))
	window.ShowAndRun()
}

type Downloader struct {
	Filename string
	Addr     string
	Client   *http.Client
	headers  []Header
	Progress *ProgressReader
	FileSize int64
}

type Header struct {
	key   string
	value string
}

func (d *Downloader) get(url string) (*http.Response, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, errors.New("Failed to create request: " + url)
	}
	req.Header.Set("User-Agent", fmt.Sprintf("%s/%s", ProgramName, Version))
	if len(d.headers) > 0 {
		for _, h := range d.headers {
			req.Header.Set(h.key, h.value)
		}
	}
	resp, err := d.Client.Do(req)
	if err != nil {
		return nil, errors.New("Failed to download: " + url)
	}
	return resp, nil
}

func (d *Downloader) getFileInfo() error {
	response, err := d.get(d.Addr)
	if err != nil {
		return errors.New(fmt.Sprint("Error downloading ", d.Addr, ": ", err))
	}
	cd := response.Header.Get("Content-Disposition")
	d.FileSize = response.ContentLength
	if err := response.Body.Close(); err != nil {
		return errors.New(fmt.Sprint("Error downloading ", d.Addr, ": ", err))
	}
	if d.Filename != "" {
		return nil
	}
	if cd != "" {
		parts := strings.Split(cd, "filename=")
		if len(parts) > 1 {
			d.Filename = strings.Trim(parts[1], "\"")
		}
	} else {
		segments := strings.Split(d.Addr, "/")
		d.Filename = segments[len(segments)-1]
	}
	return nil
}

func (d *Downloader) openFile() (*os.File, error) {
	return os.OpenFile(d.Filename, os.O_RDWR|os.O_CREATE, 0666)
}

func (d *Downloader) Download() error {
	defer func() {
		downloading = false
		d.headers = []Header{}
	}()
	if err := d.getFileInfo(); err != nil {
		return errors.New(fmt.Sprint("Error getting filename ", d.Addr, ": ", err))
	}
	fmt.Println("Filename:", d.Filename)
	var resume int64
	if info, err := os.Stat(d.Filename); err == nil {
		resume = info.Size()
	}
	if resume == d.FileSize {
		return ALREADY_DOWNLOADED_ERROR
	}
	file, err := d.openFile()
	if err != nil {
		return errors.New(fmt.Sprint("Error creating file: ", d.Addr, ": ", err))
	}
	defer file.Close()
	if resume > 0 {
		d.headers = append(d.headers, Header{key: "Range", value: fmt.Sprintf("bytes=%d-", resume)})
	}
	resp, err := d.get(d.Addr)
	if err != nil {
		return errors.New(fmt.Sprint("Error downloading ", d.Addr, ": ", err))
	}
	defer resp.Body.Close()
	total := resp.ContentLength
	d.Progress = &ProgressReader{Reader: resp.Body, Total: total}
	go func() {
		lastTime = time.Now()
		lastBytes = d.Progress.Downloaded
		time.Sleep(250 * time.Millisecond)
		for {
			if d.Progress.Downloaded == d.Progress.Total && d.Progress.Downloaded > 0 {
				return
			}
			currentSpeed = calculateBps(lastTime, lastBytes, time.Now(), d.Progress.Downloaded) / 1_000_000
			lastTime = time.Now()
			lastBytes = d.Progress.Downloaded
			time.Sleep(1000 * time.Millisecond)
		}
	}()
	if resume > 0 {
		_, err = file.Seek(resume, io.SeekStart)
		if err != nil {
			return errors.New(fmt.Sprint("Error seeking ", d.Addr, ": ", err))
		}
	}
	downloading = true
	_, err = io.Copy(file, d.Progress)
	if err != nil {
		return errors.New(fmt.Sprint("Error downloading ", d.Addr, ": ", err))
	}
	fmt.Println("\nDownload complete!")
	return nil
}

func main() {
	progressChan = make(chan ProgressMessage)
	d := &Downloader{}
	d.Client = &http.Client{}
	guiEnable := false
	flag.BoolVar(&guiEnable, "g", false, "\nenable gui")
	flag.StringVar(&d.Addr, "u", "", "url of file\n example: -u "+PlaceHolder)
	flag.StringVar(&d.Filename, "f", "", "output file name")
	flag.Parse()
	if guiEnable {
		g := GUI{
			app:        app.New(),
			Downloader: d,
		}
		g.start()
		return
	}
	if d.Addr == "" {
		fmt.Printf("Example Usage: %s -f file.ext -u %s\n", os.Args[0], PlaceHolder)
		flag.Usage()
		os.Exit(1)
	}
	go func() {
		for {
			currentProgress := <-progressChan
			percent := float64(currentProgress.Downloaded) / float64(currentProgress.Total) * 100
			fmt.Printf("\rDownloading... %s of %s %.2f%% @ %7.2fMbps",
				formatBytes(currentProgress.Downloaded),
				formatBytes(currentProgress.Total),
				percent,
				currentSpeed)
		}
	}()
	if err := d.Download(); err != nil {
		log.Fatal(err)
	}
}
