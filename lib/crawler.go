package crawler

import (
	"bytes"
	"compress/gzip"
	"encoding/base32"
	"fmt"
	"io/ioutil"
	"log"
	"math"
	"mime"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	humanize "github.com/dustin/go-humanize"
	"github.com/goware/urlx"
	"github.com/jackdanger/collectlinks"
	"github.com/jcelliott/lumber"
	"github.com/schollz/boltdb-server/connect"
)

// Crawler is the crawler instance
type Crawler struct {
	client                     *http.Client
	wg                         sync.WaitGroup
	programTime                time.Time
	curFileList                map[string]bool
	BaseURL                    string
	KeywordsToExclude          []string
	KeywordsToInclude          []string
	MaxNumberWorkers           int
	MaxNumberConnections       int
	Verbose                    bool
	FilePrefix                 string
	Remote, Username, Password string // Parameters for BoltDB remote connection
	TimeIntervalToPrintStats   int
	TimeIntervalToBackupDB     int
	numTrash                   int
	numDone                    int
	numToDo                    int
	numberOfURLSParsed         int
	conn                       *connect.Connection
	log                        *lumber.ConsoleLogger
}

func encodeURL(url string) string {
	return base32.StdEncoding.EncodeToString([]byte(url))
}

// New will create a new crawler
func New(url string, boltdbserver string, trace bool) (*Crawler, error) {
	var err error
	c := new(Crawler)
	if trace {
		c.log = lumber.NewConsoleLogger(lumber.TRACE)
	} else {
		c.log = lumber.NewConsoleLogger(lumber.WARN)
	}
	c.BaseURL = url
	c.MaxNumberConnections = 100
	c.MaxNumberWorkers = 100
	c.FilePrefix = encodeURL(url)
	c.TimeIntervalToPrintStats = 5
	c.TimeIntervalToBackupDB = 5
	c.Remote = ""
	c.log.Info("Creating new database on %s: %s.db", boltdbserver, encodeURL(url))
	c.conn, err = connect.Open(boltdbserver, encodeURL(url))
	if err != nil {
		return c, err
	}
	err = c.conn.CreateBuckets([]string{"todo", "trash", "done"})
	if err != nil {
		c.log.Error("Problem creating buckets")
		return c, err
	}
	var keys []string
	keys, err = c.conn.GetKeys("todo")
	if err != nil {
		c.log.Error("Problem getting todo list")
		return c, err
	}
	c.numToDo = len(keys)
	keys, err = c.conn.GetKeys("done")
	if err != nil {
		c.log.Error("Problem getting done list")
		return c, err
	}
	c.numDone = len(keys)
	keys, err = c.conn.GetKeys("trash")
	if err != nil {
		return c, err
	}
	c.numTrash = len(keys)

	return c, err
}

func (c *Crawler) Name() string {
	return encodeURL(c.BaseURL)
}

func (c *Crawler) GetLinks() (links []string, err error) {
	doneLinks, err := c.conn.GetAll("done")
	if err != nil {
		return links, err
	}
	todoLinks, err := c.conn.GetAll("todo")
	if err != nil {
		return links, err
	}
	links = make([]string, len(doneLinks)+len(todoLinks))
	linksI := 0
	for link := range doneLinks {
		links[linksI] = link
		linksI++
	}
	for link := range todoLinks {
		links[linksI] = link
		linksI++
	}
	return links, nil
}

func (c *Crawler) Dump() error {
	links, err := c.GetLinks()
	if err != nil {
		return err
	}

	err = ioutil.WriteFile(encodeURL(c.BaseURL)+".txt", []byte(strings.Join(links, "\n")), 0755)
	if err != nil {
		return err
	}
	fmt.Printf("Wrote %d links to %s\n", len(links), encodeURL(c.BaseURL)+".txt")
	return nil
}

func (c *Crawler) downloadOrCrawlLink(url string, currentNumberOfTries int, download bool) error {
	// Decrement the counter when the goroutine completes.
	defer c.wg.Done()

	if download {
		// Check if it is already downloaded and exists as a file
		if _, ok := c.curFileList[encodeURL(url)]; ok {
			c.log.Trace("Already downloaded %s", url)
			c.conn.Post("done", map[string]string{url: strconv.Itoa(currentNumberOfTries)})
			return nil
		}
	}

	// Try to download
	currentNumberOfTries++
	resp, err := c.client.Get(url)
	if err != nil {
		// Post to trash immedietly if the download fails
		err2 := c.conn.Post("trash", map[string]string{url: strconv.Itoa(currentNumberOfTries)})
		if err2 != nil {
			return err
		}
		c.log.Trace("Problem with %s: %s", url, err.Error())
		return nil
	}

	defer resp.Body.Close()
	if resp.StatusCode == 200 {
		c.numberOfURLSParsed++

		// Download, if downloading
		if download {
			contentType := resp.Header.Get("Content-type")
			contentTypes, contentTypeErr := mime.ExtensionsByType(contentType)
			extension := ""
			if contentTypeErr == nil {
				extension = contentTypes[0]
				if extension == ".htm" || extension == ".hxa" {
					extension = ".html"
				}
			} else {
				return err
			}
			fileContent, err := ioutil.ReadAll(resp.Body)
			if err != nil {
				return err
			}

			var buf bytes.Buffer
			writer := gzip.NewWriter(&buf)
			writer.Write(fileContent)
			writer.Close()
			filename := encodeURL(url) + extension + ".gz"
			os.Mkdir("downloaded", 0755)
			err = ioutil.WriteFile(path.Join("downloaded", filename), buf.Bytes(), 0755)
			if err != nil {
				return err
			}

			c.log.Trace("Saved %s to %s", url, encodeURL(url)+extension)
		} else {
			links := collectlinks.All(resp.Body)
			c.log.Info("Got %d links from %s\n", len(links), url)
			linkCandidates := make([]string, len(links))
			linkCandidatesI := 0
			for _, link := range links {
				// Do not use query parameters
				if strings.Contains(link, "?") {
					link = strings.Split(link, "?")[0]
				}
				// Add the Base URL to everything if it doesn't have it
				if !strings.Contains(link, "http") {
					link = c.BaseURL + link
				}
				// Skip links that have a different Base URL
				if !strings.Contains(link, c.BaseURL) {
					c.log.Trace("Skipping %s because it has a different base URL", link)
					continue
				}
				// Normalize the link
				parsedLink, _ := urlx.Parse(link)
				normalizedLink, _ := urlx.Normalize(parsedLink)
				if len(normalizedLink) == 0 {
					continue
				}

				// Exclude keywords, skip if any are found
				foundExcludedKeyword := false
				for _, keyword := range c.KeywordsToExclude {
					if strings.Contains(normalizedLink, keyword) {
						foundExcludedKeyword = true
						c.log.Trace("Skipping %s because contains %s", link, keyword)
						break
					}
				}
				if foundExcludedKeyword {
					continue
				}

				// Include keywords, skip if any are NOT found
				foundIncludedKeyword := false
				for _, keyword := range c.KeywordsToInclude {
					if strings.Contains(normalizedLink, keyword) {
						foundIncludedKeyword = true
						break
					}
				}
				if !foundIncludedKeyword && len(c.KeywordsToInclude) > 0 {
					continue
				}

				// If it passed all the tests, add to link candidates
				linkCandidates[linkCandidatesI] = normalizedLink
				linkCandidatesI++
			}
			linkCandidates = linkCandidates[0:linkCandidatesI]

			// Check to see if any link candidates have already been done
			doesHaveKeysMap, err := c.conn.HasKeys([]string{"todo", "trash", "done"}, linkCandidates)
			if err != nil {
				return err
			}
			linksToDo := make(map[string]string)
			for link, alreadyDone := range doesHaveKeysMap {
				if alreadyDone {
					continue
				}
				linksToDo[link] = "0"
				c.numToDo++
			}
			// Post new links to todo list
			c.log.Trace("Posting %d more links todo", len(linksToDo))
			err = c.conn.Post("todo", linksToDo)
			if err != nil {
				return err
			}

		}

		// Dequeue the current URL
		err = c.conn.Post("done", map[string]string{url: strconv.Itoa(currentNumberOfTries)})
		if err != nil {
			c.log.Error("Problem posting to done: %s", err.Error())
		}
		c.log.Trace("Posted %s to done", url)
		c.numDone++
		c.numToDo--
	} else {
		if currentNumberOfTries > 3 {
			// Delete this URL as it has been tried too many times
			err = c.conn.Post("trash", map[string]string{url: strconv.Itoa(currentNumberOfTries)})
			if err != nil {
				c.log.Error("Problem posting to trash: %s", err.Error())
			}
			c.numTrash++
			c.numToDo--
			c.log.Trace("Too many tries, trashing " + url)
		} else {
			// Update the URL with the number of tries
			m := map[string]string{url: strconv.Itoa(currentNumberOfTries)}
			c.conn.Post("todo", m)
		}
	}
	return nil
}

// Crawl downloads the pages specified in the todo file
func (c *Crawler) Download(urls []string) error {
	download := true

	// Determine which files have been downloaded
	c.curFileList = make(map[string]bool)
	files, err := ioutil.ReadDir("downloaded")
	if err == nil {
		for _, f := range files {
			name := strings.Split(f.Name(), ".")[0]
			if len(name) < 2 {
				continue
			}
			c.curFileList[name] = true
		}
	}

	urlsAlreadyAdded, err := c.conn.HasKeys([]string{"todo", "trash", "done"}, urls)
	if err != nil {
		return err
	}
	urlsStillToDo := make(map[string]string)
	for url, alreadyAdded := range urlsAlreadyAdded {
		if alreadyAdded {
			continue
		}
		urlsStillToDo[url] = "0"
	}
	if len(urlsStillToDo) > 0 {
		c.conn.Post("todo", urlsStillToDo)
	}

	return c.downloadOrCrawl(download)
}

// Crawl is the function to crawl with the set parameters
func (c *Crawler) Crawl() error {
	c.log.Trace("Checking to see if database has %s", c.BaseURL)
	urlsAlreadyAdded, err := c.conn.HasKeys([]string{"todo", "trash", "done"}, []string{c.BaseURL})
	if err != nil {
		return err
	}
	c.log.Trace("urlsAlreadyAdded: %v", urlsAlreadyAdded)
	urlsStillToDo := make(map[string]string)
	for url, alreadyAdded := range urlsAlreadyAdded {
		if alreadyAdded {
			continue
		}
		urlsStillToDo[url] = "0"
	}
	if len(urlsStillToDo) > 0 {
		c.log.Trace("Posting todo: %v", urlsStillToDo)
		c.conn.Post("todo", urlsStillToDo)
	}
	download := false
	return c.downloadOrCrawl(download)
}

func (c *Crawler) downloadOrCrawl(download bool) error {
	// Generate the connection pool
	tr := &http.Transport{
		MaxIdleConns:       c.MaxNumberConnections,
		IdleConnTimeout:    30 * time.Second,
		DisableCompression: true,
	}
	c.client = &http.Client{Transport: tr}

	c.programTime = time.Now()
	c.numberOfURLSParsed = 0
	it := 0
	go c.contantlyPrintStats()
	for {
		it++
		linksToDo, err := c.conn.Pop("todo", c.MaxNumberWorkers)
		if err != nil {
			return err
		}
		if len(linksToDo) == 0 {
			break
		}
		for url, numTriesStr := range linksToDo {
			numTries, err := strconv.Atoi(numTriesStr)
			if err != nil {
				return err
			}
			c.wg.Add(1)
			go c.downloadOrCrawlLink(url, numTries, download)
		}
		c.wg.Wait()

		if math.Mod(float64(it), 100) == 0 {
			// reload the configuration
			fmt.Println("Reloading the HTTP pool")
			c.client = &http.Client{Transport: tr}
		}
	}
	c.numToDo = 0
	c.printStats()
	return nil
}

func round(f float64) int {
	if math.Abs(f) < 0.5 {
		return 0
	}
	return int(f + math.Copysign(0.5, f))
}

func (c *Crawler) contantlyPrintStats() {
	for {
		time.Sleep(time.Duration(int32(c.TimeIntervalToPrintStats)) * time.Second)
		if c.numToDo == 0 {
			fmt.Println("Finished")
			return
		}
		c.printStats()
	}
}

func (c *Crawler) printStats() {
	URLSPerSecond := round(float64(c.numberOfURLSParsed) / float64(time.Since(c.programTime).Seconds()))
	log.Printf("%s parsed (%d/s), %s todo, %s done, %s trashed\n",
		humanize.Comma(int64(c.numberOfURLSParsed)),
		URLSPerSecond,
		humanize.Comma(int64(c.numToDo)),
		humanize.Comma(int64(c.numDone)),
		humanize.Comma(int64(c.numTrash)))
}
