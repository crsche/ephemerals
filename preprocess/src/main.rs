#![feature(async_closure)]

#[macro_use]
extern crate log;
extern crate pretty_env_logger;

use std::{
	fmt,
	fs::{self, File, OpenOptions},
	io::{BufReader, BufWriter, Write},
};

use anyhow::Result;
use futures::{stream, StreamExt};
use hashbrown::HashMap;
use indicatif::{ProgressBar, ProgressState, ProgressStyle};
use serde::Deserialize;
use tokio::task::JoinHandle;
use trust_dns_resolver::{config::*, error::ResolveErrorKind, TokioAsyncResolver};

const CONF_PATH: &'static str = "../config.toml";

// config.toml, used by both the preprocessor and the collector
#[derive(Deserialize)]
struct RawConfig {
	preprocess: Config,
}

// Actual config
#[derive(Deserialize)]
struct Config {
	input:               String,
	exists_out:          String,
	unexists_out:        String,
	pretty:              bool,
	concurrent_requests: usize,
}

#[tokio::main]
async fn main() -> Result<()> {
	pretty_env_logger::init();
	info!("Initialized logging");

	let raw_conf: RawConfig = toml::from_str(&fs::read_to_string(CONF_PATH)?)?;
	let conf = raw_conf.preprocess;
	info!("Loaded config from {}", CONF_PATH);

	let in_file = File::open(&conf.input)?;
	let rdr = BufReader::new(in_file);
	let input: HashMap<String, Vec<String>> = serde_json::from_reader(rdr)?;
	info!("Processed input from {}", conf.input);

	let num_hostnames = input.iter().map(|(_, v)| v.len() as u64).sum();

	// DNS resolver (reused)
	let resolver =
		TokioAsyncResolver::tokio(ResolverConfig::cloudflare(), ResolverOpts::default())?;

	// Main "objects" representing existing and unexisting hostnames in the same
	// form as the input
	let mut exists = HashMap::with_capacity(input.len());
	let mut unexists = HashMap::with_capacity(input.len());

	// Iterator over input map
	let mut it = input.into_iter();

	info!("Beginning data collection");
	let pb = ProgressBar::new(num_hostnames);
	pb.set_message("Starting");
	pb.set_style(
		ProgressStyle::with_template(
			"{spinner:.green} [{elapsed_precise}] [{msg:^25}] [{wide_bar:.cyan/blue}] \
			 [{human_pos}/{human_len}] — [{per_sec} ({eta})]",
		)
		.unwrap()
		.with_key("eta", |state: &ProgressState, w: &mut dyn fmt::Write| {
			write!(w, "{:.1}s", state.eta().as_secs_f64()).unwrap()
		})
		.progress_chars("=>-"),
	);

	// Split based on hostname existence
	while let Some((category, hostnames)) = it.next() {
		// Caches for existing and unexisting hosts
		let mut existing = Vec::with_capacity(hostnames.len());
		let mut unexisting = Vec::with_capacity(hostnames.len());

		// Determine if hostnamess exist
		let mut st = stream::iter(hostnames)
			.map(|hostname| {
				let resolver = &resolver;
				let pb = pb.clone();
				async move {
					pb.set_message(hostname.clone());
					let resp = resolver.lookup_ip(format!("{}.", &hostname)).await;
					let exists = match resp {
						Ok(_) => true,
						Err(e) => {
							match e.kind() {
								ResolveErrorKind::NoRecordsFound { .. } => false,
								_ => {
									info!("{}: {}", hostname, e.kind());
									false
								},
							}
						}
					};

					debug!("{} ok: {}", &hostname, exists);
					pb.inc(1);
					(hostname, exists)
				}
			})
			.buffer_unordered(conf.concurrent_requests);

		// Insert the hostnames into the local vectors depending on if they exist
		while let Some((hostname, exists)) = st.next().await {
			if exists {
				existing.push(hostname)
			} else {
				unexisting.push(hostname)
			}
		}
		debug!("{}: finished", &category);
		// Add the vectors to the output object
		exists.insert(category.clone(), existing);
		unexists.insert(category, unexisting);
	}
	pb.finish();
	info!("Finished data collection");

	let num_exists: usize = exists.iter().map(|(_, v)| v.len()).sum();
	info!("{}/{} exist", num_exists, num_hostnames);

	let num_unexists: usize = unexists.iter().map(|(_, v)| v.len()).sum();
	info!("{}/{} don't exist", num_unexists, num_hostnames);

	// Write output objects to file in parallel
	let exists_saving: JoinHandle<Result<()>> = tokio::spawn(async move {
		let mut exists_out = OpenOptions::new()
			.write(true)
			.create(true)
			.open(&conf.exists_out)?;
		let mut exists_wrtr = BufWriter::new(&mut exists_out);
		if conf.pretty {
			serde_json::to_writer_pretty(&mut exists_wrtr, &exists)?;
		} else {
			serde_json::to_writer(&mut exists_wrtr, &exists)?;
		}
		exists_wrtr.flush()?;
		info!("Wrote output for existing to {}", conf.exists_out);
		Ok(())
	});

	let unexists_saving: JoinHandle<Result<()>> = tokio::spawn(async move {
		let mut unexists_out = OpenOptions::new()
			.write(true)
			.create(true)
			.open(&conf.unexists_out)?;
		let mut unexists_wrtr = BufWriter::new(&mut unexists_out);
		if conf.pretty {
			serde_json::to_writer_pretty(&mut unexists_wrtr, &unexists)?;
		} else {
			serde_json::to_writer(&mut unexists_wrtr, &unexists)?;
		}
		unexists_wrtr.flush()?;
		info!("Wrote output for unexisting to {}", conf.unexists_out);
		Ok(())
	});

	// Wait for output to be written
	exists_saving.await??;
	unexists_saving.await??;

	Ok(())
}
