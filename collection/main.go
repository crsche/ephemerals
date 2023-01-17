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

	"github.com/BurntSushi/toml"
)

const (
	CONF_PATH = "../config.toml"
)

type (
	// TOML configuration
	Config struct {
		Collection struct {
			Sites string `toml:"sites_in"`
			Db    struct {
				Uri        string
				Name       string
				Collection string
			}
			Browser struct {
				Path    string
				MaxTabs int `toml:"max_tabs"`
				Timeout float64
			}
			Dns struct {
				ConfPath string `toml:"conf_path"`
			}
		}
	}

	// Raw JSON input {category: [url]}
	RawInput map[string][]string
	// Flattened JSON: [{url, category}]
	Input []InputSite
	// Because no tuples :(
	InputSite struct {
		Category string
		Url      string
	}

	// Represents a single top-level site in the database
	Site struct {
		// Top-level site url
		Url string
		// What category does the site belong to?
		Category string
		// Collection of trials for this URL
		Trials []Trial
	}
	Trial struct {
		// Browser version string
		BrowserVersion string `bson:"browser_version"`
		// Time at which the trial was **started**
		StartTime time.Time `bson:"start_time"`
		// List of requests observed by trial
		//
		// There can and **will** be duplicates
		Requests []Request
	}

	Request struct {
		// URL of the request
		Url      u.URL
		HostName string
		// Type of request
		ResourceType string `bson:"resource_type"`
		TTL          uint32
	}
)

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

// Form numChunks chunks from array
func (in Input) Chunks(numChunks int) (chunks [][]InputSite) {
	for i := 0; i < numChunks; i++ {
		min := (i * len(in) / numChunks)
		max := ((i + 1) * len(in)) / numChunks
		chunks = append(chunks, in[min:max])
	}
	return chunks
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

func getQueue(sites []InputSite, b *playwright.Browser, timeout float64, collection *mongo.Collection, dnsClient *dns.Client, dnsConf *dns.ClientConfig, wg *sync.WaitGroup) {
	for _, site := range sites {
		getSite(site.Url, site.Category, b, timeout, collection, dnsClient, dnsConf)
	}
	wg.Done()
}

// Gets data for a given site and inserts it into the database
func getSite(url string, category string, b *playwright.Browser, timeout float64, collection *mongo.Collection, dnsClient *dns.Client, dnsConf *dns.ClientConfig) {
	LOG.Infof("%s: starting data collection", url)
	url = "https://" + url

	p, e := (*b).NewPage()
	if e != nil {
		LOG.Errorf("%s: failed to create page: %v", url, e)
	}
	var reqs []Request
	p.On("request", func(req playwright.Request) {
		LOG.Debugf("%s -> %s", url, req.URL())

		parsedUrl, e := u.ParseRequestURI(req.URL())
		if e != nil {
			LOG.Warnf("%s: failed to parse hostname of request to `%s`: %v", url, req.URL(), e)
		} else {
			ttl, e := getTTL(dnsClient, dnsConf, parsedUrl)
			if e != nil {
				LOG.Warnf("%s: failed to get ttl of request to `%s`: %v", url, parsedUrl.Host, e)
			}
			reqs = append(reqs, Request{Url: *parsedUrl, HostName: parsedUrl.Host, ResourceType: req.ResourceType(), TTL: ttl})
		}
	})

	// Load page
	start := time.Now()
	_, e = p.Goto(url)
	if e != nil {
		LOG.Errorf("%s: failed to fully load: %v", url, e)
	} else {
		_, e := p.WaitForNavigation(playwright.PageWaitForNavigationOptions{WaitUntil: playwright.WaitUntilStateNetworkidle, Timeout: &timeout})
		LOG.Warnf("%s: failed to wait for network idle: %v", url, e)
	}

	LOG.Debugf("%s: got %d requests", url, len(reqs))

	// Construct trial
	trial := Trial{BrowserVersion: p.Context().Browser().Version(), StartTime: start, Requests: reqs}

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
	p.Close()
}

// ! GLOBAL LOGGER
var LOG *zap.SugaredLogger

func main() {
	//! Init logging
	// TODO: Change config for prod
	config := zap.NewDevelopmentConfig()
	config.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder // Enable color
	config.Level.SetLevel(zapcore.InfoLevel)
	rawLog, e := config.Build()
	if e != nil {
		log.Panicf("Failed to build logger config: %v", e)
	}
	LOG = rawLog.Sugar()
	LOG.Info("Initialized logger")

	var conf Config
	_, e = toml.DecodeFile(CONF_PATH, &conf)
	if e != nil {
		LOG.Panicf("Failed to load TOML file at %s: %v", CONF_PATH, e)
	}

	//! Load sites file
	f, e := os.Open(conf.Collection.Sites)
	if e != nil {
		LOG.Panicf("Failed to open sites file at %s: %v", conf.Collection.Sites, e)
	}
	var categories RawInput
	e = json.NewDecoder(f).Decode(&categories)
	if e != nil {
		LOG.Panicf("Failed to load JSON from %s into struct: %v", conf.Collection.Sites, e)
	}
	tabGroups := categories.Flatten().Chunks(conf.Collection.Browser.MaxTabs)
	LOG.Infof("Got site list from %s", conf.Collection.Sites)

	//! Init DB
	// Actual init
	dbCtx := context.Background()
	dbClient, e := mongo.Connect(dbCtx, options.Client().ApplyURI(conf.Collection.Db.Uri))
	if e != nil {
		LOG.Panicf("Failed to connect to DB: %v", e)
	}

	// Verify DB connection
	e = dbClient.Ping(dbCtx, readpref.Primary())
	if e != nil {
		LOG.Panicf("Client couldn't connect to the DB: %v", e)
	}
	LOG.Infof("Connected to MongoDB with URI %s", conf.Collection.Db.Uri)
	// Connect to specific site Debugrmation collection
	collection := dbClient.Database(conf.Collection.Db.Name).Collection(conf.Collection.Db.Collection)
	LOG.Infof("Connected to `%s` collection on `%s` DB", conf.Collection.Db.Collection, conf.Collection.Db.Name)

	//! Init playwright & browser
	// Start Playwright
	pw, e := playwright.Run()
	if e != nil {
		LOG.Panicf("Failed to start playwright: %v", e)
	}
	LOG.Info("Started Playwright")
	var browser playwright.Browser
	// Start browser
	if conf.Collection.Browser.Path != "" {
		LOG.Infof("Using browser path: %s", conf.Collection.Browser.Path)
		browser, e = pw.Chromium.Launch(playwright.BrowserTypeLaunchOptions{ExecutablePath: &conf.Collection.Browser.Path})
	} else {
		LOG.Info("Defaulting to default Playwright browser")
		browser, e = pw.Chromium.Launch()
	}
	if e != nil {
		LOG.Panicf("Failed to launch browser: %v", e)
	}
	LOG.Infof("Launched Chromium version %s", browser.Version())

	//! Init DNS client
	dnsConf, e := dns.ClientConfigFromFile(conf.Collection.Dns.ConfPath)
	if e != nil {
		LOG.Panicf("Failed to load DNS config from %s: %v", conf.Collection.Dns.ConfPath, e)
	}
	if len(dnsConf.Servers) < 1 {
		LOG.Panic("DNS conf contained no servers")
	}
	LOG.Infof("Using DNS server of %s", dnsConf.Servers[0])
	c := dns.Client{}

	//! Data collection
	LOG.Info("Starting data collection")
	LOG.Info(tabGroups)
	var wg sync.WaitGroup
	for i, sites := range tabGroups {
		LOG.Infof("Starting tab %d", i)
		wg.Add(1)
		go getQueue(sites, &browser, conf.Collection.Browser.Timeout, collection, &c, dnsConf, &wg)
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