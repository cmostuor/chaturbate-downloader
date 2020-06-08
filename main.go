package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/teacat/pathx"

	"github.com/grafov/m3u8"
	"github.com/parnurzeal/gorequest"
	"github.com/urfave/cli"
)

// chaturbateURL is the base url of the website.
const chaturbateURL = "https://chaturbate.com/"

// retriesAfterOnlined tells the retries for stream when disconnected but not really offlined.
var retriesAfterOnlined = 0

// lastCheckOnline logs the last check time.
var lastCheckOnline = time.Now()

// bucket stores the used segment to prevent fetched the duplicates.
var bucket []string

// segmentIndex is current stored segment index.
var segmentIndex int

//
var (
	errInternal   = errors.New("err")
	errNoUsername = errors.New("chaturbate-dvr: channel username required with `-u [username]` argument")
)

// roomDossier is the struct to parse the HLS source from the content body.
type roomDossier struct {
	HLSSource string `json:"hls_source"`
}

type getval struct {
	username   string
	filename   string
	proxyurl   string
	masterFile *os.File
}

// unescapeUnicode escapes the unicode from the content body.
func unescapeUnicode(raw string) string {
	str, err := strconv.Unquote(strings.Replace(strconv.Quote(string(raw)), `\\u`, `\u`, -1))
	if err != nil {
		panic(err)
	}
	return str
}

// getChannelURL returns the full channel url to the specified user.
func (gv *getval) getChannelURL() string {
	return fmt.Sprintf("%s%s", chaturbateURL, gv.username)
}

// getBody gets the channel page content body.
func (gv *getval) getBody() string {
	var body string

	if gv.proxyurl != "" {
		_, body, _ = gorequest.New().Proxy(gv.proxyurl).Get(gv.getChannelURL()).End()
	} else {
		_, body, _ = gorequest.New().Get(gv.getChannelURL()).End()
	}

	return body
}

// getOnlineStatus check if the user is currently online by checking the playlist exists in the content body or not.
func (gv *getval) getOnlineStatus() bool {
	return strings.Contains(gv.getBody(), "playlist.m3u8")
}

// getHLSSource extracts the playlist url from the room detail page body.
func getHLSSource(body string) (string, string) {
	// Get the room data from the page body.
	r := regexp.MustCompile(`window\.initialRoomDossier = "(.*?)"`)
	matches := r.FindAllStringSubmatch(body, -1)

	// Extract the data and get the HLS source URL.
	var roomData roomDossier
	data := unescapeUnicode(matches[0][1])
	err := json.Unmarshal([]byte(data), &roomData)
	if err != nil {
		panic(err)
	}

	return roomData.HLSSource, strings.TrimRight(roomData.HLSSource, "playlist.m3u8")
}

// parseHLSSource parses the HLS table and return the maximum resolution m3u8 source.
func parseHLSSource(url string, baseURL string) string {
	_, body, _ := gorequest.New().Get(url).End()

	// Decode the HLS table.
	p, _, _ := m3u8.DecodeFrom(strings.NewReader(body), true)
	master := p.(*m3u8.MasterPlaylist)
	return fmt.Sprintf("%s%s", baseURL, master.Variants[len(master.Variants)-1].URI)
}

// parseM3U8Source gets the current segment list, the channel might goes offline if 403 was returned.
func parseM3U8Source(url string) (chunks []*m3u8.MediaSegment, wait float64, err error) {
	resp, body, errs := gorequest.New().Get(url).End()
	// Retry after 3 seconds if the connection lost or status code returns 403 (the channel might went offline).
	if len(errs) > 0 || resp.StatusCode == http.StatusForbidden {
		return nil, 3, errInternal
	}

	// Decode the segment table.
	p, _, _ := m3u8.DecodeFrom(strings.NewReader(body), true)
	media := p.(*m3u8.MediaPlaylist)
	wait = media.TargetDuration / 1.5

	// Ignore the empty segments.
	for _, v := range media.Segments {
		if v != nil {
			chunks = append(chunks, v)
		}
	}
	return
}

// capture captures the specified channel streaming.
func (gv *getval) capture() {
	// Define the video filename by current time.
	filename := time.Now().Format("2006-01-02_15.04.05")
	fullfilename := filename + ".ts"
	masterFileparh := gv.username + "/" + fullfilename
	
	gv.filename = filename
	
	// Get the channel page content body.
	body := gv.getBody()
	if body != "" {
		// Get the master playlist URL from extracting the channel body.
		hlsSource, baseURL := getHLSSource(body)
		//log.Printf("hlsSource: %s\nbaseURL: %s", hlsSource, baseURL)
		// Get the best resolution m3u8 by parsing the HLS source table.
		m3u8Source := parseHLSSource(hlsSource, baseURL)
		// Create the master video file.
		masterFile, _ := os.OpenFile(masterFileparh, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0777)
		gv.masterFile = masterFile

		//
		log.Printf("视频将保存为 \"%s\".", fullfilename)

		go gv.combineSegment()
		gv.watchStream(m3u8Source, baseURL)
	}
}

// watchStream watches the stream and ends if the channel went offline.
func (gv *getval) watchStream(m3u8Source string, baseURL string) {
	// Keep fetching the stream chunks until the playlist cannot be accessed after retried x times.
	for {
		// Get the chunks.
		chunks, wait, err := parseM3U8Source(m3u8Source)
		// Exit the fetching loop if the channel went offline.
		if err != nil {
			if retriesAfterOnlined > 10 {
				log.Printf("重试后未能获取视频片段, 可能 %s 掉线了.", gv.username)
				break
			} else {
				log.Printf("未能获取视频片段, 重新获取(%d/10). ", retriesAfterOnlined)
				retriesAfterOnlined++
				// Wait to fetch the next playlist.
				<-time.After(time.Duration(wait*1000) * time.Millisecond)
				continue
			}
		}
		if retriesAfterOnlined != 0 {
			log.Printf("%s 重新上线!", gv.username)
			retriesAfterOnlined = 0
		}
		for _, v := range chunks {
			// Ignore the duplicated chunks.
			if isDuplicateSegment(v.URI) {
				continue
			}
			segmentIndex++
			go gv.fetchSegment(v, baseURL, segmentIndex)
		}
		<-time.After(time.Duration(wait*1000) * time.Millisecond)
	}
}

// isDuplicateSegment returns true if the segment is already been fetched.
func isDuplicateSegment(URI string) bool {
	for _, v := range bucket {
		if URI[len(URI)-10:] == v {
			return true
		}
	}
	bucket = append(bucket, URI[len(URI)-10:])
	return false
}

// combineSegment combines the segments to the master video file in the background.
func (gv *getval) combineSegment() {
	index := 1
	var retry int
	<-time.After(4 * time.Second)

	for {
		<-time.After(300 * time.Millisecond)

		if index >= segmentIndex {
			<-time.After(1 * time.Second)
			continue
		}

		if !pathx.Exists(fmt.Sprintf("%s/%s~%d.ts", gv.username, gv.filename, index)) {
			if retry >= 5 {
				index++
				retry = 0
				continue
			}
			if retry != 0 {
				log.Printf("不能查找到 %d 片段, 会再试一次. (%d/5)", index, retry)
			}
			retry++
			<-time.After(time.Duration(1*retry) * time.Second)
			continue
		}
		if retry != 0 {
			retry = 0
		}
		//
		b, _ := ioutil.ReadFile(fmt.Sprintf("%s/%s~%d.ts", gv.username, gv.filename, index))
		gv.masterFile.Write(b)
		log.Printf("插入 %d 片段到主文件. (总计: %d)", index, segmentIndex)
		//
		os.Remove(fmt.Sprintf("%s/%s~%d.ts", gv.username, gv.filename, index))
		//
		index++
	}
}

// fetchSegment fetches the segment and append to the master file.
func (gv *getval) fetchSegment(segment *m3u8.MediaSegment, baseURL string, index int) {
	_, body, _ := gorequest.New().Get(fmt.Sprintf("%s%s", baseURL, segment.URI)).EndBytes()
	log.Printf("获取 %s (大小: %d)\n", segment.URI, len(body))
	if len(body) == 0 {
		log.Printf("跳过 %s 这个空白的片段!\n", segment.URI)
		return
	}
	//
	f, err := os.OpenFile(fmt.Sprintf("%s/%s~%d.ts", gv.username, gv.filename, index), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0777)
	if err != nil {
		panic(err)
	}
	if _, err := f.Write(body); err != nil {
		panic(err)
	}
}

func IsExist(f string) bool {
	_, err := os.Stat(f)
	return err == nil || os.IsExist(err)
}

// endpoint implements the application main function endpoint.
func endpoint(c *cli.Context) error {
	var gv getval
	gv.username = c.String("username")
	gv.proxyurl = c.String("proxyurl")

	if gv.username == "" {
		log.Fatal(errNoUsername)
	}

	fmt.Println(" .o88b. db   db  .d8b.  d888888b db    db d8888b. d8888b.  .d8b.  d888888b d88888b")
	fmt.Println("d8P  Y8 88   88 d8' `8b `~~88~~' 88    88 88  `8D 88  `8D d8' `8b `~~88~~' 88'")
	fmt.Println("8P      88ooo88 88ooo88    88    88    88 88oobY' 88oooY' 88ooo88    88    88ooooo")
	fmt.Println("8b      88~~~88 88~~~88    88    88    88 88`8b   88~~~b. 88~~~88    88    88~~~~~")
	fmt.Println("Y8b  d8 88   88 88   88    88    88b  d88 88 `88. 88   8D 88   88    88    88.")
	fmt.Println(" `Y88P' YP   YP YP   YP    YP    ~Y8888P' 88   YD Y8888P' YP   YP    YP    Y88888P")
	fmt.Println("d8888b. db    db d8888b.")
	fmt.Println("88  `8D 88    88 88  `8D")
	fmt.Println("88   88 Y8    8P 88oobY'")
	fmt.Println("88   88 `8b  d8' 88`8b")
	fmt.Println("88  .8D  `8bd8'  88 `88.")
	fmt.Println("Y8888D'    YP    88   YD")
	fmt.Println("---")

	if !IsExist(c.String("username")) {
		err := os.Mkdir(c.String("username"), os.ModePerm)
		if err != nil {
			fmt.Println(err)
		}
	}

	for {
		// Capture the stream if the user is currently online.
		if gv.getOnlineStatus() {
			log.Printf("用户: %s 在线! 开始获取...", gv.username)
			gv.capture()
			segmentIndex = 0
			bucket = []string{}
			retriesAfterOnlined = 0
			continue
		}
		// Otherwise we keep checking the channel status until the user is online.
		log.Printf("用户: %s 不在线, %d分钟后再次检查...", gv.username, c.Int("interval"))
		<-time.After(time.Minute * time.Duration(c.Int("interval")))
	}
	return nil
}

func main() {
	//app := &cli.App{
	//	Flags: []cli.Flag{
	//		&cli.StringFlag{
	//			Name:    "username",
	//			Aliases: []string{"u"},
	//			Value:   "",
	//			Usage:   "channel username to watching",
	//		},
	//		&cli.IntFlag{
	//			Name:    "interval",
	//			Aliases: []string{"i"},
	//			Value:   1,
	//			Usage:   "minutes to check if a channel goes online or not",
	//		},
	//	},
	//	Name:   "chaturbate-dvr",
	//	Usage:  "watching a specified chaturbate channel and auto saves the stream as local file",
	//	Action: endpoint,
	//}

	app := cli.NewApp()
	app.Name = "chaturbate-dvr"
	app.EnableBashCompletion = true
	app.Action = endpoint
	app.Usage = "监视指定的chaturbate通道和自动保存流作为本地文件"
	app.Flags = []cli.Flag{
		cli.IntFlag{
			Name:        "interval,i",
			Usage:       "设置检查用户是否上线的时间",
			EnvVar:      "",
			FilePath:    "",
			Required:    false,
			Hidden:      false,
			Value:       1,
			Destination: nil,
		},
		cli.StringFlag{
			Name:        "username,u",
			Usage:       "设置要观看的用户名",
			EnvVar:      "",
			FilePath:    "",
			Required:    false,
			Hidden:      false,
			TakesFile:   false,
			Value:       "",
			Destination: nil,
		},
		cli.StringFlag{
			Name:        "proxyurl,p",
			Usage:       "设置代理地址",
			EnvVar:      "",
			FilePath:    "",
			Required:    false,
			Hidden:      false,
			TakesFile:   false,
			Value:       "",
			Destination: nil,
		},
	}

	err := app.Run(os.Args)
	if err != nil {
		log.Fatal(err)
	}
}
