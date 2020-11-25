package main

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/eknkc/pug"
)

const help string = `usage:
	this <arg1> [<arg2> [<arg3]]

	arg1:	account name (identical to the file in the ./opml folder) OR
		"all" to generate an output for all (as index.html)
	arg2:	video # to check for the latest videos of a channel, default 5
	arg3:	true/false -> get duration for each video (slow!), default: false
	
account name may contain a '#' suffix to indicate
initial selection in the rendered html file`

const defaultEntriesPerChannel int = 5
const lastNDays int = 7

type accountChannels struct {
	account  string
	selected bool
	blurred  bool
	channels []channel
}

type channel struct {
	name string
	url  string
}

type youtubeVideo struct {
	Account      string
	Channel      string
	Title        string
	VideoID      string
	DurationStr  string
	TimestampStr string
	timestamp    time.Time
}

type youtubeAccountGroup struct {
	Selected bool
	Blurred  bool
	Videos   []youtubeVideo
}

type youtubeVideoSlice []youtubeVideo

func (s youtubeVideoSlice) Len() int {
	return len(s)
}
func (s youtubeVideoSlice) Less(i, j int) bool {
	return s[i].timestamp.After(s[j].timestamp)
}
func (s youtubeVideoSlice) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

func readAccountChannels(exeDir, account string) (accountChannels, error) {
	xmlData, err := ioutil.ReadFile(exeDir + "/opml/" + account)
	if err != nil {
		return accountChannels{}, err
	}

	type Outline struct {
		XMLName  xml.Name  `xml:"outline"`
		Outlines []Outline `xml:"outline"`
		Title    string    `xml:"title,attr"`
		URL      string    `xml:"xmlUrl,attr"`
	}
	type Body struct {
		XMLName xml.Name `xml:"body"`
		Outline Outline  `xml:"outline"`
	}
	type Opml struct {
		XMLName xml.Name `xml:"opml"`
		Body    Body     `xml:"body"`
	}

	var opml Opml
	xml.Unmarshal(xmlData, &opml)

	var ret accountChannels
	ret.account = strings.Title(strings.TrimSuffix(strings.TrimSuffix(account, "#"), "!"))
	ret.selected = strings.HasSuffix(account, "#")
	ret.blurred = strings.HasSuffix(account, "!")
	ret.channels = make([]channel, len(opml.Body.Outline.Outlines))
	for idx := range opml.Body.Outline.Outlines {
		ret.channels[idx].name = opml.Body.Outline.Outlines[idx].Title
		ret.channels[idx].url = opml.Body.Outline.Outlines[idx].URL
	}

	return ret, nil
}

func (y *youtubeVideo) parseTimestamp(location *time.Location) error {
	datetime, err := time.Parse(time.RFC3339, y.TimestampStr)
	if err != nil {
		return err
	}
	y.timestamp = datetime.In(location)
	y.TimestampStr = y.timestamp.Format("2006-01-02 15:04:05")
	return nil
}

func getDurationStr(videoID string) string {
	resp, err := http.Get("http://youtube.com/get_video_info?video_id=" + videoID)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return ""
	}
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return ""
	}
	re, err := regexp.Compile("lengthSeconds(%[0-9A-Fa-f]{2}){3}([^%]+)")
	if err != nil {
		return ""
	}
	res := re.FindStringSubmatch(string(body))
	dur, _ := strconv.Atoi(res[len(res)-1])
	sec := dur % 60
	dur /= 60
	min := dur % 60
	dur /= 60
	if dur > 0 {
		return fmt.Sprintf("%02d:%02d:%02d", dur, min, sec)
	}
	return fmt.Sprintf("%02d:%02d", min, sec)
}

func fetch(account string, channel channel, wg *sync.WaitGroup, youtubeVideoChan chan *youtubeVideo, timestampCutoff *time.Time, maxEntries int, fetchDuration bool) {
	defer wg.Done()

	type entry struct {
		XMLName   xml.Name `xml:"entry"`
		Title     string   `xml:"title"`
		VideoID   string   `xml:"videoId"`
		Published string   `xml:"published"`
	}
	type feed struct {
		XMLName xml.Name `xml:"feed"`
		Entries []entry  `xml:"entry"`
	}

	resp, err := http.Get(channel.url)
	if err != nil {
		fmt.Println("Channel:", channel.name, "--Error: get() error!")
		fmt.Println(err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		fmt.Println("Channel:", channel.name, "--Error:", resp.Status)
		return
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		fmt.Println("Channel:", channel.name, "--Error: readall error!")
		fmt.Println(err)
		return
	}
	var f feed
	err = xml.Unmarshal(body, &f)
	if err != nil {
		fmt.Println("Channel:", channel.name, "--Error: unmarshal error!")
		fmt.Println(err)
		return
	}

	for idx, item := range f.Entries {
		if idx >= maxEntries {
			break
		}
		var yv youtubeVideo
		yv.Account = account
		yv.Channel = channel.name
		yv.Title = item.Title
		yv.VideoID = item.VideoID
		if fetchDuration {
			yv.DurationStr = getDurationStr(item.VideoID)
		}
		yv.TimestampStr = item.Published
		err = yv.parseTimestamp(timestampCutoff.Location())
		if err != nil {
			fmt.Println("Channel:", channel.name, "Video:", item.VideoID, "--Error: parsing timestamp-string", yv.TimestampStr)
			continue
		}
		if yv.timestamp.Before(*timestampCutoff) {
			break
		}
		youtubeVideoChan <- &yv
	}
}

func renderAndVisit(exeDir string, youtubeAccountsMap map[string]*youtubeAccountGroup) {
	templateFile, templateError := ioutil.ReadFile(exeDir + "/template.pug")
	if templateError != nil {
		fmt.Println("Error: reading template.pug")
		fmt.Println(templateError)
		return
	}
	template, templateError := pug.CompileString(string(templateFile), pug.Options{})
	if templateError != nil {
		fmt.Println("Error: compiling template.pug")
		fmt.Println(templateError)
		return
	}
	var buf bytes.Buffer
	if err := template.Execute(io.Writer(&buf), map[string]interface{}{
		"accounts": youtubeAccountsMap,
	}); err != nil {
		fmt.Println("Error: executing template.pug")
		fmt.Println(err)
		return
	}
	os.MkdirAll(exeDir + "/out/", os.ModePerm)
	if err := ioutil.WriteFile(exeDir+"/out/index.html", buf.Bytes(), 0644); err != nil {
		fmt.Println("Error: outputting index")
		fmt.Println(err)
		return
	}
	cmd := exec.Command("xdg-open", exeDir+"/out/index.html")
	cmd.Run()
}

func printHelp() {
	fmt.Println(help)
}

func main() {
	exeDir := filepath.Dir(os.Args[0])
	accountsFilenames, err := filepath.Glob(exeDir + "/opml/*")

	if err != nil {
		fmt.Println("could not read account opml files in ./opml/")
		return
	}
	if len(os.Args) >= 2 && len(os.Args) <= 4 {
		account := ""
		for _, a := range accountsFilenames {
			b := filepath.Base(a)
			if os.Args[1] == b {
				account = os.Args[1]
				break
			}
			if os.Args[1]+"#" == b {
				account = os.Args[1] + "#"
				break
			}
		}
		checkAllIntoOne := false
		if account == "" {
			switch os.Args[1] {
			case "all":
				checkAllIntoOne = true
			default:
				fmt.Println("Error: could not find account (arg1) in opml folder!")
				return
			}
		}
		entries := defaultEntriesPerChannel
		if len(os.Args) == 3 {
			a2, err := strconv.Atoi(os.Args[2])
			if err != nil {
				fmt.Printf("Error: could not parse count (arg2), using default %d\n", entries)
			} else {
				entries = a2
			}
		}
		fetchDuration := false
		if len(os.Args) == 4 {
			fetchDuration = os.Args[3] == "true"
		}
		var accounts []accountChannels
		if checkAllIntoOne {
			accounts = make([]accountChannels, 0, len(accountsFilenames))
			for _, accFilename := range accountsFilenames {
				acc, err := readAccountChannels(exeDir, filepath.Base(accFilename))
				if err != nil {
					fmt.Println("Error: could not parse opml file, ignoring file")
					continue
				}
				accounts = append(accounts, acc)
			}
		} else {
			a, err := readAccountChannels(exeDir, account)
			if err != nil {
				fmt.Println("Error: could not parse opml file")
				return
			}
			a.selected = true
			accounts = []accountChannels{a}
		}
		youtubeAccountsMap := make(map[string]*youtubeAccountGroup)
		for idx := range accounts {
			youtubeAccountsMap[accounts[idx].account] = &youtubeAccountGroup{
				Selected: accounts[idx].selected,
				Blurred:  accounts[idx].blurred,
				Videos:   make([]youtubeVideo, 0, len(accounts[idx].channels)),
			}
		}

		youtubeVideoChan := make(chan *youtubeVideo, 1)
		var wg sync.WaitGroup
		done := make(chan int, 1)

		dateCutoff := time.Now().AddDate(0, 0, -lastNDays)
		for idx := range accounts {
			for idx2 := range accounts[idx].channels {
				wg.Add(1)
				go fetch(accounts[idx].account, accounts[idx].channels[idx2], &wg, youtubeVideoChan, &dateCutoff, entries, fetchDuration)
			}
		}
		go func() {
			wg.Wait()
			close(done)
		}()

		isDone := false
		for !isDone {
			select {
			case <-done:
				isDone = true
				break
			case vid := <-youtubeVideoChan:
				youtubeAccountsMap[vid.Account].Videos = append(youtubeAccountsMap[vid.Account].Videos, *vid)
			}
		}
		for idx := range accounts {
			sort.Sort(youtubeVideoSlice(youtubeAccountsMap[accounts[idx].account].Videos))
		}
		renderAndVisit(exeDir, youtubeAccountsMap)
	} else {
		printHelp()
	}
}
