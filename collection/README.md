# Collection

This program takes a collection of websites and stores their resource requests in a database.

## Dependencies

- [Go](https://go.dev/doc/install)
- [Playwright](https://playwright.dev/docs/intro) version of Chromium.
- [MongoDB](https://www.mongodb.com/try/download/community)

### Playwright

Install Playwright using the following command (if you run into problems, remove the `--with-deps` flag):

`go run github.com/playwright-community/playwright-go/cmd/playwright install --with-deps`

## Configuration

Please look under `[collection]` in the [config.toml](../config.toml) file.

## To run

`go run main.go`

You can set the log level in the config if it's too messy.

## TODO

- Write this in Rust because it's most likely way more efficient :D.
