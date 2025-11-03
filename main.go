package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/PuerkitoBio/goquery"
	utls "github.com/refraction-networking/utls"
)

// --- MISSION PARAMETERS ---
const (
	WORKER_COUNT          = 1000
	REQUEST_TIMEOUT       = 10 * time.Second
	BASE_URL              = "https://www.futbin.com/players"
	OUTPUT_FILE           = "futbin_players_full.json"
	
	// These constants are kept for reference but the main loop is removed.
	LOOP_DELAY            = 5 * time.Minute
	FULL_SCRAPE_INTERVAL  = 12 * time.Hour
	FULL_SCRAPE_PAGES     = 659
	QUICK_SCRAPE_PAGES    = 200
)

// --- STATIC PROXY POOL ---
var STATIC_PROXIES = []string{
	"104.207.46.133:3129", "104.207.37.201:3129", "65.111.15.251:3129", "45.3.38.43:3129",
	"216.26.232.130:3129", "216.26.253.115:3129", "209.50.188.10:3129", "154.213.160.165:3129",
	"65.111.20.96:3129", "65.111.20.114:3129", "104.207.39.180:3129", "209.50.177.147:3129",
	"104.207.50.243:3129", "65.111.27.6:3129", "65.111.30.87:3129", "45.3.48.166:3129",
	"45.3.50.165:3129", "65.111.10.7:3129", "209.50.190.91:3129", "65.111.28.100:3129",
	"209.50.162.110:3129", "45.3.55.55:3129", "65.111.12.65:3129", "104.207.54.155:3129",
	"209.50.171.183:3129", "209.50.171.27:3129", "216.26.242.154:3129", "216.26.236.92:3129",
	"154.213.162.191:3129", "209.50.175.49:3129", "216.26.234.71:3129", "209.50.169.71:3129",
	"45.3.46.45:3129", "45.3.42.44:3129", "104.207.43.170:3129", "65.111.30.72:3129",
	"216.26.254.36:3129", "216.26.252.143:3129", "65.111.11.135:3129", "216.26.241.236:3129",
	"216.26.255.118:3129", "209.50.178.11:3129", "209.50.187.119:3129", "104.207.36.97:3129",
	"209.50.173.71:3129", "216.26.229.38:3129", "45.3.51.54:3129", "154.213.165.175:3129",
	"45.3.40.131:3129", "104.207.46.11:3129", "65.111.14.118:3129", "104.207.51.92:3129",
	"104.167.25.122:3129", "45.3.35.170:3129", "216.26.231.114:3129", "65.111.8.229:3129",
	"209.50.170.45:3129", "209.50.174.42:3129", "104.207.37.144:3129", "45.3.40.179:3129",
	"216.26.226.5:3129", "209.50.185.122:3129", "216.26.225.156:3129", "216.26.255.178:3129",
	"209.50.178.43:3129", "154.213.164.57:3129", "216.26.224.185:3129", "104.207.54.47:3129",
	"154.213.161.213:3129", "104.207.55.212:3129", "45.3.46.79:3129", "216.26.250.72:3129",
	"216.26.240.123:3129", "216.26.230.127:3129", "45.3.33.57:3129", "216.26.231.247:3129",
	"65.111.1.6:3129", "45.3.47.12:3129", "104.207.50.10:3129", "216.26.235.26:3129",
	"154.213.162.82:3129", "209.50.180.193:3129", "104.207.42.206:3129", "45.3.38.201:3129",
	"216.26.241.118:3129", "65.111.21.32:3129", "154.213.161.244:3129", "65.111.8.82:3129",
	"45.3.46.61:3129", "154.213.166.175:3129", "209.50.185.0:3129", "216.26.249.73:3129",
	"45.3.53.142:3129", "209.50.161.115:3129", "65.111.12.140:3129", "216.26.226.109:3129",
	"104.207.61.14:3129", "154.213.161.41:3129", "209.50.163.229:3129", "104.207.54.126:3129",
}

var clientProfiles = []utls.ClientHelloID{
	utls.HelloChrome_120,
	utls.HelloFirefox_120,
	utls.HelloSafari_16_0,
}

// --- DATA STRUCTURES ---
type Player struct {
	Name        string `json:"name"`
	Rating      int    `json:"rating"`
	Position    string `json:"position"`
	PSPrice     string `json:"ps_price"`
	PCPrice     string `json:"pc_price"`
	Skills      string `json:"skills"`
	WeakFoot    string `json:"weak_foot"`
	Pace        int    `json:"pace"`
	Shooting    int    `json:"shooting"`
	Passing     int    `json:"passing"`
	Dribbling   int    `json:"dribbling"`
	Defending   int    `json:"defending"`
	Physicality int    `json:"physicality"`
}

type Task struct {
	PageURL      string
	AttemptCount int
}

// --- SIMPLIFIED PROXY MANAGER ---
type ProxyManager struct {
	proxies []string
	next    uint64
}

func NewProxyManager(proxies []string) *ProxyManager {
	return &ProxyManager{proxies: proxies}
}

func (pm *ProxyManager) GetProxy() string {
	idx := atomic.AddUint64(&pm.next, 1) - 1
	return pm.proxies[idx%uint64(len(pm.proxies))]
}

// --- CORE FUNCTIONS ---
func createUTLSClient(proxyURL *url.URL) *http.Client {
	profile := clientProfiles[rand.Intn(len(clientProfiles))]

	return &http.Client{
		Timeout: REQUEST_TIMEOUT,
		Transport: &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
			DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				tcpConn, err := (&net.Dialer{Timeout: REQUEST_TIMEOUT}).DialContext(ctx, network, addr)
				if err != nil {
					return nil, err
				}
				config := &utls.Config{
					InsecureSkipVerify: true,
				}
				uconn := utls.UClient(tcpConn, config, profile)
				if err := uconn.Handshake(); err != nil {
					tcpConn.Close()
					return nil, err
				}
				return uconn, nil
			},
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, 
		},
	}
}

func parsePlayerData(htmlContent []byte) []Player {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(htmlContent))
	if err != nil { return nil }
	var players []Player
	doc.Find("tr.player-row").Each(func(i int, s *goquery.Selection) {
		players = append(players, Player{
			Name:        strings.TrimSpace(s.Find("a.table-player-name").Text()),
			Rating:      toInt(s.Find("div.rating-square").Text()),
			Position:    strings.TrimSpace(s.Find("div.table-pos-main").Text()),
			PSPrice:     strings.TrimSpace(s.Find("td.platform-ps-only > div.price").Text()),
			PCPrice:     strings.TrimSpace(s.Find("td.platform-pc-only > div.price").Text()),
			Skills:      strings.TrimSpace(s.Find("td.table-skills").Text()),
			WeakFoot:    strings.TrimSpace(s.Find("td.table-weak-foot").Text()),
			Pace:        toInt(s.Find("td.table-pace div").Text()),
			Shooting:    toInt(s.Find("td.table-shooting div").Text()),
			Passing:     toInt(s.Find("td.table-passing div").Text()),
			Dribbling:   toInt(s.Find("td.table-dribbling div").Text()),
			Defending:   toInt(s.Find("td.table-defending div").Text()),
			Physicality: toInt(s.Find("td.table-physicality div").Text()),
		})
	})
	return players
}

func blitzWorker(id int, taskChan <-chan Task, requeueChan chan<- Task, resultsChan chan<- []Player, pm *ProxyManager, wg *sync.WaitGroup) {
	defer wg.Done()
	for task := range taskChan {
		proxyAddr := pm.GetProxy()
		proxyURL, err := url.Parse("http://" + proxyAddr)
		if err != nil {
			requeueChan <- task 
			continue
		}

		client := createUTLSClient(proxyURL)
		req, _ := http.NewRequest("GET", task.PageURL, nil)
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
		req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7")
		req.Header.Set("Accept-Language", "en-US,en;q=0.9")
		req.Header.Set("HX-Request", "true")
		req.Header.Set("HX-Target", "players_table_body")

		resp, err := client.Do(req)
		if err != nil {
			requeueChan <- task
			continue
		}

		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()

		if readErr != nil || resp.StatusCode != 200 || !strings.Contains(string(body), "player-row") {
			requeueChan <- task
			continue
		}

		players := parsePlayerData(body)
		if len(players) > 0 {
			resultsChan <- players
		} else {
			requeueChan <- task
		}
	}
}

// --- SCRAPE CYCLE ---
func runScrapeCycle(pagesToScrape int, scrapeType string) {
	startTime := time.Now()
	log.Printf("--- uTLS BLITZ: INITIATING %s SCRAPE WITH PREMIUM PROXIES ---", scrapeType)
	rand.Seed(time.Now().UnixNano())

	rand.Shuffle(len(STATIC_PROXIES), func(i, j int) {
		STATIC_PROXIES[i], STATIC_PROXIES[j] = STATIC_PROXIES[j], STATIC_PROXIES[i]
	})
	proxyManager := NewProxyManager(STATIC_PROXIES)

	taskChan := make(chan Task, pagesToScrape)
	requeueChan := make(chan Task, pagesToScrape*2)
	resultsChan := make(chan []Player, pagesToScrape)
	
	go func() {
		for task := range requeueChan {
			if task.AttemptCount < 5 {
				task.AttemptCount++
				taskChan <- task
			}
		}
	}()

	var wg sync.WaitGroup
	log.Printf("[ASSAULT] Launching %d workers for %d pages using %d premium proxies.\n", WORKER_COUNT, pagesToScrape, len(STATIC_PROXIES))
	for i := 0; i < WORKER_COUNT; i++ {
		wg.Add(1)
		go blitzWorker(i, taskChan, requeueChan, resultsChan, proxyManager, &wg)
	}

	for i := 1; i <= pagesToScrape; i++ {
		taskChan <- Task{PageURL: fmt.Sprintf("%s?page=%d", BASE_URL, i)}
	}

	var allPlayers []Player
	pagesDone := 0
	for pagesDone < pagesToScrape {
		pageData := <-resultsChan
		allPlayers = append(allPlayers, pageData...)
		pagesDone++
		fmt.Printf("\r[PROGRESS] Pages Secured: %d/%d | Total Players: %d", pagesDone, pagesToScrape, len(allPlayers))
	}

	close(taskChan)
	wg.Wait() 
	close(requeueChan)

	endTime := time.Now()
	fmt.Printf("\n[COMPLETE] Pages Secured: %d/%d | Total Players: %d\n", pagesDone, pagesToScrape, len(allPlayers))
	log.Println("==================================================")
	log.Printf("--- %s SCRAPE COMPLETE ---", scrapeType)
	log.Printf("Total Duration: %.2f seconds.\n", endTime.Sub(startTime).Seconds())
	log.Printf("Total Players Found: %d\n", len(allPlayers))

	file, _ := json.MarshalIndent(allPlayers, "", "  ")
	os.WriteFile(OUTPUT_FILE, file, 0644)
	log.Printf("[OUTPUT] Saved %d players to '%s'\n", len(allPlayers), OUTPUT_FILE)
}

// --- MAIN SCHEDULER (MODIFIED FOR AUTOMATION) ---
func main() {
	// For automated runs, we perform one targeted scrape and then exit.
	// The scheduling is handled by the external workflow (GitHub Actions).
	// We will use the "QUICK_SCRAPE_PAGES" constant for frequent updates.
	// You can change this to FULL_SCRAPE_PAGES if the schedule is less frequent.
	log.Println("AGENT: Initiating automated single-run scrape cycle.")
	runScrapeCycle(QUICK_SCRAPE_PAGES, "AUTOMATED")
	log.Println("AGENT: Automated cycle complete. Terminating.")
}

func toInt(s string) int {
	i, _ := strconv.Atoi(strings.TrimSpace(s))
	return i
}