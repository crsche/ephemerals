package main

import (
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"net"
	u "net/url"

	"context"
	"encoding/json"

	"github.com/miekg/dns"

	"github.com/playwright-community/playwright-go"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// ! Config
const (
	MONGO_URI       string = "mongodb://127.0.0.1:27017"
	DB_NAME         string = "ephemerals"
	COLLECTION_NAME string = "sites"

	// Maximum number of tabs
	MAX_TABS int = 16
	// Path to a **Playwright** browser
	BROWSER_PATH string = "/home/cone/.cache/ms-playwright/chromium-1041/chrome-linux/chrome"
	// How long to wait in addition to "networkidle"
	LOAD_WAIT time.Duration = time.Duration(0) * time.Second

	// Where to load our DNS client (for TTLs) config from
	RESOLV_CONF string = "/etc/resolv.conf"

	// Where to find the input website data
	SITES_FILE string = "sites.json"
)

type RawInput map[string][]string
type Input []InputSite
type InputSite struct {
	category string
	url      string
}

// Flatten categories
func (in RawInput) Flatten() Input {
	var res Input
	for c, urls := range in {
		for _, url := range urls {
			res = append(res, InputSite{c, url})
		}
	}
	return res
}

func (in Input) Chunks(size int) (chunks [][]InputSite) {
	for size < len(in) {
		in, chunks = in[size:], append(chunks, in[0:size:size])
	}
	return append(chunks, in)
}

// Represents a single top-level site in the database
type Site struct {
	// Top-level site url
	Url string `bson:"url"`
	// What category does the site belong to?
	Category string `bson:"category"`
	// Collection of trials for this URL
	Trials []Trial `bson:"trials"`
}
type Trial struct {
	// Browser version string
	BrowserVersion string `bson:"browser_version"`
	// Time at which the trial was **started**
	StartTime time.Time `bson:"start_time"`
	// List of requests observed by trial
	//
	// There can and **will** be duplicates
	Requests []Request `bson:"requests"`
}

type Request struct {
	// URL of the request
	Url u.URL `bson:"url"`
	// Type of request
	ResourceType string `bson:"resource_type"`
	TTL          uint32 `bson:"ttl"`
}

// Get TTL info for a hostname
func getTTL(c *dns.Client, conf *dns.ClientConfig, url *u.URL) (uint32, error) {
	m := dns.Msg{}
	m.SetQuestion(url.Host+".", dns.TypeA)
	r, _, e := c.Exchange(&m, net.JoinHostPort(conf.Servers[0], conf.Port))
	if e != nil {
		return 0, e
	} else if len(r.Answer) < 1 {
		return 0, fmt.Errorf("DNS response contained no answers")
	} else {
		return r.Answer[0].Header().Ttl, nil
	}
}

// // Gets data for a given category and inserts it into the database
// func getCategory(urls *[]string, category string, browser *playwright.Browser, collection *mongo.Collection, dnsClient *dns.Client, dnsConf *dns.ClientConfig, wg *sync.WaitGroup) {
// 	var wg2 sync.WaitGroup
// 	urlBuffer := chunks(*urls, MAX_TABS)
// 	for i, urlGroup := range urlBuffer {
// 		LOG.Infof("Starting tab group %d", i)
// 		for _, url := range urlGroup {
// 			page, e := b.NewPage()
// 			if e != nil {
// 				LOG.Panic("%s: Failed to create new page: %v", url, e)
// 			}
// 			wg2.Add(1)
// 			go getSite(url, category, &page, collection, dnsClient, dnsConf, &wg2)
// 		}
// 		wg2.Wait()
// 	}
// 	wg.Done()
// }

func getQueue(sites []InputSite, b *playwright.Browser, collection *mongo.Collection, dnsClient *dns.Client, dnsConf *dns.ClientConfig, wg *sync.WaitGroup) {
	page, e := (*b).NewPage()
	if e != nil {
		LOG.Errorf("Failed to create page: %v", e)
	}
	for _, site := range sites {
		getSite(site.url, site.category, &page, collection, dnsClient, dnsConf)
	}
	wg.Done()
}

// Gets data for a given site and inserts it into the database
func getSite(url string, category string, p *playwright.Page, collection *mongo.Collection, dnsClient *dns.Client, dnsConf *dns.ClientConfig) {
	LOG.Infof("%s: starting data collection", url)
	url = "https://" + url

	var reqs []Request
	(*p).On("request", func(req playwright.Request) {
		LOG.Debugf("%s -> %s", url, req.URL())

		parsedUrl, e := u.ParseRequestURI(req.URL())
		if e != nil {
			LOG.Errorf("%s: failed to parse hostname of request to `%s`: %v", url, req.URL(), e)
		} else {
			ttl, e := getTTL(dnsClient, dnsConf, parsedUrl)
			if e != nil {
				LOG.Errorf("%s: failed to get ttl of request to `%s`: %v", url, parsedUrl.Host, e)
			}
			reqs = append(reqs, Request{Url: *parsedUrl, ResourceType: req.ResourceType(), TTL: ttl})
		}
	})

	// Load page
	start := time.Now()
	_, e := (*p).Goto(url)
	if e != nil {
		LOG.Warnf("%s: failed to fully load: %v", url, e)
	} else {
		(*p).WaitForLoadState("networkidle")
	}
	elapsed := time.Since(start)
	LOG.Debugf("%s: paged loaded (`networkidle`), took %fs", url, elapsed.Seconds())
	LOG.Debugf("%s: waiting an additional %fs", url, LOAD_WAIT.Seconds())
	(*p).WaitForTimeout(float64(LOAD_WAIT.Milliseconds()))
	LOG.Debugf("%s: finished waiting", url)
	LOG.Debugf("%s: got %d requests", url, len(reqs))

	// Construct trial
	trial := Trial{BrowserVersion: (*p).Context().Browser().Version(), StartTime: start, Requests: reqs}
	// Stop load
	(*p).Goto("about:blank")

	True := true // So we can reference
	// Insert or update the new trial Debugrmation
	res, e := collection.UpdateOne(context.Background(), bson.D{{"url", url}, {"category", category}}, bson.D{{"$push", bson.D{{"trials", trial}}}}, &options.UpdateOptions{Upsert: &True})
	if e != nil {
		LOG.Errorf("%s: failed to update trial: %v", url, e)
	}
	if res.UpsertedID != nil {
		LOG.Infof("%s: created new db entry", url)
	}
	LOG.Infof("%s: inserted new trial", url)
}

var (
	// ! GLOBAL LOGGER
	LOG *zap.SugaredLogger
)

func main() {
	//! Init logging
	// TODO: Change config for prod
	config := zap.NewDevelopmentConfig()
	config.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder // Enable color
	rawLog, e := config.Build()
	if e != nil {
		log.Panicf("Failed to build logger config: %v", e)
	}
	LOG = rawLog.Sugar()
	LOG.Info("Initialized logger")

	//! Load sites file
	f, e := os.Open(SITES_FILE)
	if e != nil {
		LOG.Panicf("Failed to open sites file at %s: %v", SITES_FILE, e)
	}
	var categories RawInput
	e = json.NewDecoder(f).Decode(&categories)
	if e != nil {
		LOG.Panicf("Failed to load JSON from %s into struct: %v", SITES_FILE, e)
	}
	sitesBuffered := categories.Flatten().Chunks(MAX_TABS)
	LOG.Infof("Got site list from %s", SITES_FILE)

	//! Init DB
	// Actual init
	dbCtx := context.Background()
	dbClient, e := mongo.Connect(dbCtx, options.Client().ApplyURI(MONGO_URI))
	if e != nil {
		LOG.Panicf("Failed to connect to DB: %v", e)
	}

	// Verify DB connection
	e = dbClient.Ping(dbCtx, readpref.Primary())
	if e != nil {
		LOG.Panicf("Client couldn't connect to the DB: %v", e)
	}
	LOG.Infof("Connected to MongoDB with URI %s", MONGO_URI)
	// Connect to specific site Debugrmation collection
	collection := dbClient.Database(DB_NAME).Collection(COLLECTION_NAME)
	LOG.Infof("Connected to `%s` collection on `%s` DB", COLLECTION_NAME, DB_NAME)

	//! Init playwright & browser
	// Start Playwright
	pw, e := playwright.Run()
	if e != nil {
		LOG.Panicf("Failed to start playwright: %v", e)
	}
	LOG.Info("Started Playwright")

	// Start browser
	path := BROWSER_PATH
	browser, e := pw.Chromium.Launch(playwright.BrowserTypeLaunchOptions{ExecutablePath: &path}) //! Browser customization here
	if e != nil {
		LOG.Panicf("Failed to launch browser: %v", e)
	}

	LOG.Infof("Launched Chromium version %s", browser.Version())

	//! Init DNS client
	dnsConf, e := dns.ClientConfigFromFile(RESOLV_CONF)
	if e != nil {
		LOG.Panicf("Failed to load DNS config from %s: %v", RESOLV_CONF, e)
	}
	if len(dnsConf.Servers) < 1 {
		LOG.Panic("DNS conf contained no servers")
	}
	LOG.Infof("Using DNS server of %s", dnsConf.Servers[0])
	c := dns.Client{}

	//! Data collection
	// tabs := make(chan playwright.Page, MAX_TABS)
	// for i := 0; i < MAX_TABS; i++ {
	// 	p, e := browser.NewPage()
	// 	if e != nil {
	// 		LOG.Panicf("Failed to create page %d: %v", i, e)
	// 	}
	// 	tabs <- p
	// }
	LOG.Info("Starting data collection")
	var wg sync.WaitGroup
	for i, sites := range sitesBuffered {
		LOG.Infof("Starting browser chunk %d", i)
		wg.Add(1)
		go getQueue(sites, &browser, collection, &c, dnsConf, &wg)
	}
	wg.Wait()

	//! End of program
	e = browser.Close()
	if e != nil {
		LOG.Panicf("Failed close browser: %v", e)
	}
	LOG.Info("Closed browser")

	e = pw.Stop()
	if e != nil {
		LOG.Panicf("Failed to stop Playwright: %v", e)
	}
	LOG.Info("Stopped Playwright")

	e = dbClient.Disconnect(dbCtx)
	if e != nil {
		LOG.Panicf("Failed to disconnect from DB: %v", e)
	}
	LOG.Info("Disconnected from DB")

	e = rawLog.Sync()
	if e != nil {
		LOG.Panicf("Failed to sync log buffers: %v", e)
	}
}
