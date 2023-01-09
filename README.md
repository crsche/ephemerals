# Ephemerals

This is a basic program for "ephemeral" URL research. It is still in early development so you probably **shouldn't** run it. It basically runs through a list of websites and stores their requests.

## Dependencies

- [Go](https://go.dev/doc/install)
- [Playwright](https://playwright.dev/docs/intro) version of Chromium.
- [MongoDB](https://www.mongodb.com/try/download/community)

## Configuration

Configuration is done using the `.env` file, you can find an example [here](.env-example)
You **must** provide a `BROWSER_PATH` and `SITES_FILE` path.

- `BROWSER_PATH` should be something like `/home/{YOU}/.cache/ms-playwright/chromium-xxxx/chrome-linux/chrome`
- `SITES_FILE` should be the path to the sites you would like to collect data for. This file should be in the following format:
  ```json
  {category: [url1, url2, url3, ...]}
  ```
  - **NOTE**: The urls should **include** `http[s]://`
- `MAX_TABS` Dictates the maximum number of tabs per browser window at once.
- Default storage location is `ephemerals` ⇒ `sites` in MongoDB but it can be changed with the `DB_NAME` and `COLLECTION_NAME` vars.

## Running

To run the script:

```sh
go run main.go
```

## TODO

- Add packet sniffing with [GoPacket](https://github.com/google/gopacket) to capture TTL values.
- Clean up and optimize using channels and by reusing browser resources.

