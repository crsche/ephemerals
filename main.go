package main

import (
	"log"
	"os"
	"sync"
	"time"

	// "time"

	"context"
	"encoding/json"

	// "github.com/go-rod/rod"
	// "github.com/go-rod/rod/lib/proto"
	// "github.com/go-rod/stealth"
	// "sync"
	"github.com/playwright-community/playwright-go"
	// "go.mongodb.org/mongo-driver/bson"
	"github.com/joho/godotenv"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"strconv"
)

// ! DEFAULTS
const (
	DEFAULT_MONGO_URI       string = "mongodb://127.0.0.1:27017"
	DEFAULT_DB_NAME         string = "ephemerals"
	DEFAULT_COLLECTION_NAME string = "sites"
	//! CHANGE THIS
	DEFAULT_BROWSER_PATH string = "/home/cone/.cache/ms-playwright/chromium-1041/chrome-linux/chrome"
	DEFAULT_SITES_FILE   string = "sites.json"
	DEFAULT_MAX_TABS     int    = 32
)

type Input map[string][]string

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
	Url string `bson:"url"`
	// Type of request
	ResourceType string `bson:"resource_type"`
}

func chunks[T any](items []T, chunkSize int) (chunks [][]T) {
	for chunkSize < len(items) {
		items, chunks = items[chunkSize:], append(chunks, items[0:chunkSize:chunkSize])
	}
	return append(chunks, items)
}

// Gets data for a given category and inserts it into the database
func getCategory(urls []string, category string, bCtx playwright.BrowserContext, collection *mongo.Collection, wg *sync.WaitGroup, tabsPerCtx int) {
	defer wg.Done()
	var wg2 sync.WaitGroup

	urlBuffer := chunks(urls, tabsPerCtx)

	for i, urlGroup := range urlBuffer {
		LOG.Infof("Starting tab group %d", i)
		for _, url := range urlGroup {
			page, e := bCtx.NewPage()
			if e != nil {
				LOG.Panic("Failed to initialize new page for %s(%s): %v", url, category, e)
			}
			wg2.Add(1)
			go getSite(url, category, page, collection, &wg2)
		}
		wg2.Wait()
	}
}

// Gets data for a given site and inserts it into the database
func getSite(url string, category string, page playwright.Page, collection *mongo.Collection, wg *sync.WaitGroup) {
	defer wg.Done()
	defer page.Close()

	LOG.Infof("Getting data for %s", url)

	var reqs []Request

	page.On("request", func(req playwright.Request) {
		// LOG.Debugf("Request from %s to %s", url, req.URL())
		reqs = append(reqs, Request{Url: req.URL(), ResourceType: req.ResourceType()})
	})

	start := time.Now()
	_, e := page.Goto(url)

	if e != nil {
		LOG.Warnf("Failed to fully load %s, still inserting existing requests: %v", url, e)
	} else {
		page.WaitForLoadState("networkidle")
	}
	elapsed := time.Since(start)

	LOG.Infof("Loaded %s, took %fs", url, elapsed.Seconds())
	LOG.Infof("Got %d requests for %s", len(reqs), url)

	trial := Trial{BrowserVersion: page.Context().Browser().Version(), StartTime: start, Requests: reqs}

	True := true // So we can reference
	// TODO: Use chan and UpdateMany for efficiency?
	// TODO: Use different context here?
	res, e := collection.UpdateOne(context.Background(), bson.D{{"url", url}, {"category", category}}, bson.D{{"$push", bson.D{{"trials", trial}}}}, &options.UpdateOptions{Upsert: &True})
	if e != nil {
		LOG.Errorf("Failed to update trial for %s: %v", url, e)
	}
	if res.UpsertedID != nil {
		LOG.Debugf("Upserted for %s", url)
	}
	LOG.Infof("Inserted new trial for %s", url)
}

// ! GLOBAL LOGGER
var LOG *zap.SugaredLogger

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

	//! Load .env
	e = godotenv.Load()
	if e != nil {
		LOG.Warnf("Failed to load .env file, falling back to defaults: %v", e)
	}
	LOG.Info("Loaded .env successfully")

	//! Load sites file
	//Env vars
	SITES_FILE, found := os.LookupEnv("SITES_FILE")
	if !found {
		LOG.Warnf("SITES_FILE not found in .env or env, using default of %s", DEFAULT_SITES_FILE)
		SITES_FILE = DEFAULT_SITES_FILE
	}
	rawJson, e := os.ReadFile(SITES_FILE)
	if e != nil {
		LOG.Panicf("Failed to read sites file at %s: %v", SITES_FILE, e)
	}
	var categories Input
	json.Unmarshal(rawJson, &categories)
	LOG.Infof("Got site list from %s", SITES_FILE)

	//! Init DB
	// Env vars
	MONGO_URI, found := os.LookupEnv("MONGO_URI")
	if !found {
		LOG.Warnf("MONGO_URI not found in .env or env, using default of %s", DEFAULT_MONGO_URI)
		MONGO_URI = DEFAULT_MONGO_URI
	}
	DB_NAME, found := os.LookupEnv("DB_NAME")
	if !found {
		LOG.Warnf("DB_NAME not found in .env or env, using default of %s", DEFAULT_DB_NAME)
		DB_NAME = DEFAULT_DB_NAME
	}
	COLLECTION_NAME, found := os.LookupEnv("COLLECTION_NAME")
	if !found {
		LOG.Warnf("COLLECTION_NAME not found in .env or env, using default of %s", DEFAULT_COLLECTION_NAME)
		COLLECTION_NAME = DEFAULT_COLLECTION_NAME
	}
	// Actual init
	dbCtx := context.Background()
	dbClient, e := mongo.Connect(dbCtx, options.Client().ApplyURI(MONGO_URI))
	if e != nil {
		LOG.Panic("Failed to connect to DB: ", e)
	}
	defer func() {
		if e := dbClient.Disconnect(dbCtx); e != nil {
			LOG.Panic("Failed to disconnect from DB: ", e)
		}
	}()
	// Verify DB connection
	e = dbClient.Ping(dbCtx, readpref.Primary())
	if e != nil {
		LOG.Panic("Client couldn't connect to the DB: ", e)
	}
	LOG.Infof("Connected to MongoDB with URI %s", MONGO_URI)
	// Connect to specific site information collection
	collection := dbClient.Database(DB_NAME).Collection(COLLECTION_NAME)
	LOG.Infof("Connected to `%s` collection on `%s` DB", COLLECTION_NAME, DB_NAME)

	//! Init playwright & browser
	// Env vars
	BROWSER_PATH, found := os.LookupEnv("BROWSER_PATH")
	if !found {
		LOG.Warnf("BROWSER_PATH not found in .env or env, using default of %s", DEFAULT_BROWSER_PATH)
		BROWSER_PATH = DEFAULT_BROWSER_PATH
	}
	// Start Playwright
	pw, e := playwright.Run()
	if e != nil {
		LOG.Panic("Failed to start playwright: ", e)
	}
	defer pw.Stop()
	LOG.Info("Started Playwright")
	// Start browser
	path := BROWSER_PATH
	browser, e := pw.Chromium.Launch(playwright.BrowserTypeLaunchOptions{ExecutablePath: &path}) //! Browser customization here
	if e != nil {
		LOG.Panic("Failed to launch Chromium instance ", e)
	}
	defer func() {
		if e = browser.Close(); e != nil {
			LOG.Panic("Failed to close browser: ", e)
		}
	}()
	LOG.Infof("Launched Chromium version %s", browser.Version())

	//! Data collection
	//Env vars
	var MAX_TABS int
	rawMaxTabs, found := os.LookupEnv("MAX_TABS")
	if found {
		MAX_TABS, e = strconv.Atoi(rawMaxTabs)
		if e != nil {
			LOG.Warnf("MAX_TABS value of %s in .env is not a number, falling back to default of %d", rawMaxTabs, DEFAULT_MAX_TABS)
		}
	} else {
		LOG.Warnf("MAX_TABS not found in .env or env, using default of %s", DEFAULT_MAX_TABS)
		MAX_TABS = DEFAULT_MAX_TABS
	}
	LOG.Info("Starting data collection")
	var wg sync.WaitGroup
	for category, sites := range categories {
		bCtx, e := browser.NewContext() //! Browser customization here
		if e != nil {
			LOG.Panicf("Failed to create browser context for %s: %v", category, e)
		}
		wg.Add(1)
		go getCategory(sites, category, bCtx, collection, &wg, MAX_TABS)
	}
	wg.Wait()
}
