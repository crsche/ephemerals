# Preprocessing

This program takes a file of hostnames specified by [config.toml](../config.toml) and splits it into two files: one with the "existing"/reachable hostnames and one with the unreachable ones. The paths to both of these files are specified in the [config.toml](../config.toml) file. The results are **NOT GUARANTEED**.

## Dependencies

- [Rust 🦀](https://www.rust-lang.org/tools/install)

## How?

The program uses [trust_dns_resolver](https://docs.rs/trust-dns-resolver/latest/trust_dns_resolver/) to resolve hostnames. If the resolver receives a `NoRecordsFound` response, it deems the hostname to not exist. Otherwise, it's considered to exist.

## Configuration

Please look under `[preprocess]` in the [config.toml](../config.toml) file.

## TODO

- Hone the criteria for what indicates whether the hostname doesn't exist (based on a DNS response).