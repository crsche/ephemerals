package main

import (
	"log"
	"os"
	// "runtime"
	"sync"
	"time"

	u "net/url"

	"context"
	"encoding/json"

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

	// Maximum number of concurrent browsers contexts
	MAX_BROWSERS int    = 8
	// Maximum number of tabs per browser context
	MAX_TABS     int    = 32
	// Path to a **Playwright** browser
	BROWSER_PATH string = "/home/cone/.cache/ms-playwright/chromium-1041/chrome-linux/chrome"

	// Where to find the input website data
	SITES_FILE string = "test.json"

	// How long to wait in addition to "networkidle"
	LOAD_WAIT time.Duration = time.Duration(30) * time.Second
)

type Input map[string][]string

func (in Input) Chunks(size int) []Input {
	res := make([]Input, len(in)/size)
	chunk := make(Input, size)
	i := 0
	for k, v := range in {
		chunk[k] = v
		i++
		if i == size || i == len(in) {
			i = 0
			res = append(res, chunk)
			chunk = make(Input, size)
		}
	}
	return res
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
	// TTL          uint32 `bson:"ttl"`
}

func chunks[T any](items []T, chunkSize int) (chunks [][]T) {
	for chunkSize < len(items) {
		items, chunks = items[chunkSize:], append(chunks, items[0:chunkSize:chunkSize])
	}
	return append(chunks, items)
}

// Gets data for a given category and inserts it into the database
func getCategory(urls *[]string, category string, bCtx *playwright.BrowserContext, collection *mongo.Collection, wg *sync.WaitGroup) {
	defer wg.Done()
	var wg2 sync.WaitGroup

	urlBuffer := chunks(*urls, MAX_TABS)

	for i, urlGroup := range urlBuffer {
		LOG.Debugf("Starting tab group %d", i)
		for _, url := range urlGroup {
			page, e := (*bCtx).NewPage()
			if e != nil {
				LOG.Panic("%s: Failed to create new page: %v", url, e)
			}
			wg2.Add(1)
			go getSite(url, category, &page, collection, &wg2)
		}
		wg2.Wait()
	}
}

// Gets data for a given site and inserts it into the database
func getSite(url string, category string, page *playwright.Page, collection *mongo.Collection, wg *sync.WaitGroup) {
	defer wg.Done()
	defer (*page).Close()

	LOG.Debugf("%s: starting data collection", url)

	var reqs []Request

	(*page).On("request", func(req playwright.Request) {
		LOG.Debugf("%s -> %s", url, req.URL())
		parsedUrl, e := u.ParseRequestURI(req.URL())
		if e != nil {
			LOG.Errorf("%s: failed to parse request to `%s`: %v", url, req.URL(), e)
		} else {
			//! Get TTL Debug here
			reqs = append(reqs, Request{Url: *parsedUrl, ResourceType: req.ResourceType()})
		}
	})

	// Load page
	start := time.Now()
	_, e := (*page).Goto(url)

	if e != nil {
		LOG.Warnf("%s: failed to fully load: %v", url, e)
	} else {
		(*page).WaitForLoadState("networkidle")
	}
	elapsed := time.Since(start)
	LOG.Debugf("%s: paged loaded (`networkidle`), took %fs", url, elapsed.Seconds())
	LOG.Debugf("%s: waiting an additional %fs", url, LOAD_WAIT.Seconds())
	time.Sleep(LOAD_WAIT)
	LOG.Debugf("%s: finished waiting", url)
	LOG.Debugf("%s: got %d requests", url, len(reqs))

	// Construct trial
	trial := Trial{BrowserVersion: (*page).Context().Browser().Version(), StartTime: start, Requests: reqs}

	True := true // So we can reference
	// Insert or update the new trial Debugrmation
	res, e := collection.UpdateOne(context.Background(), bson.D{{"url", url}, {"category", category}}, bson.D{{"$push", bson.D{{"trials", trial}}}}, &options.UpdateOptions{Upsert: &True})
	if e != nil {
		LOG.Errorf("%s: failed to update trial: %v", url, e)
	}
	if res.UpsertedID != nil {
		LOG.Debugf("%s: created new db entry", url)
	}
	LOG.Debugf("%s: inserted new trial", url)
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
	defer rawLog.Sync()
	LOG.Info("Initialized logger")

	//! Load sites file
	rawJson, e := os.ReadFile(SITES_FILE)
	if e != nil {
		LOG.Panicf("Failed to read sites file at %s: %v", SITES_FILE, e)
	}
	var categories Input
	json.Unmarshal(rawJson, &categories)
	LOG.Infof("Got site list from %s", SITES_FILE)

	//! Init DB
	// Actual init
	dbCtx := context.Background()
	dbClient, e := mongo.Connect(dbCtx, options.Client().ApplyURI(MONGO_URI))
	if e != nil {
		LOG.Panicf("Failed to connect to DB: %v", e)
	}
	defer func() {
		if e := dbClient.Disconnect(dbCtx); e != nil {
			LOG.Panicf("Failed to disconnect from DB: %v", e)
		}
	}()

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
	defer pw.Stop()
	LOG.Info("Started Playwright")

	// Start browser
	path := BROWSER_PATH
	browser, e := pw.Chromium.Launch(playwright.BrowserTypeLaunchOptions{ExecutablePath: &path}) //! Browser customization here
	if e != nil {
		LOG.Panicf("Failed to launch browser: %v", e)
	}
	defer func() {
		if e = browser.Close(); e != nil {
			LOG.Panicf("Failed to close browser: %v", e)
		}
	}()
	LOG.Infof("Launched Chromium version %s", browser.Version())

	//! Data collection
	browserChunks := categories.Chunks(MAX_BROWSERS)

	LOG.Info("Starting data collection")
	var wg sync.WaitGroup
	for i, chunk := range browserChunks {
		LOG.Debugf("Starting browser chunk %d", i)
		for category, sites := range chunk {
			bCtx, e := browser.NewContext() //! Browser customization here
			if e != nil {
				LOG.Panicf("Failed to create browser context for %s: %v", category, e)
			}
			wg.Add(1)
			go getCategory(&sites, category, &bCtx, collection, &wg)
		}
		wg.Wait()
	}
}
