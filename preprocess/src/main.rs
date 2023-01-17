#![feature(async_closure)]

#[macro_use]
extern crate log;
extern crate pretty_env_logger;

use std::{
	env,
	fs::{self, File, OpenOptions},
	io::{BufReader, BufWriter, Write},
	sync::{Arc, Mutex},
};

use anyhow::Result;
use futures::{stream, StreamExt};
use hashbrown::HashMap;
use pbr::ProgressBar;
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
	env::set_var("RUST_LOG", "info");
	pretty_env_logger::init();
	info!("Initialized logging");

	let raw_conf: RawConfig = toml::from_str(&fs::read_to_string(CONF_PATH)?)?;
	let conf = raw_conf.preprocess;
	info!("Loaded config from {}", CONF_PATH);

	let in_file = File::open(format!("../{}", conf.input))?;
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
	let pb = Arc::new(Mutex::new(ProgressBar::new(num_hostnames)));
	// Split based on hostname existence
	while let Some((category, hostnames)) = it.next() {
		// Caches for existing and unexisting hosts
		let mut existing = Vec::with_capacity(hostnames.len());
		let mut unexisting = Vec::with_capacity(hostnames.len());

		// Determine if hostnamess exist
		let mut st = stream::iter(hostnames)
			.map(|hostname| {
				let pb = &pb;
				let resolver = &resolver;
				async move {
					let resp = resolver.lookup_ip(format!("{}.", &hostname)).await;
					let exists = match resp {
						Ok(_) => true,
						Err(e) => {
							debug!("{}: DNS lookup returned error: {}", &hostname, e);
							match e.kind() {
								ResolveErrorKind::NoRecordsFound { .. } => false,
								_ => true,
							}
						}
					};

					debug!("{} ok: {}", &hostname, exists);
					pb.lock().unwrap().inc();
					(hostname, exists)
				}
			})
			.buffered(conf.concurrent_requests);

		// Insert the hostnames into the local vectors depending on if they exist
		while let Some((hostname, exists)) = st.next().await {
			if exists {
				existing.push(hostname)
			} else {
				unexisting.push(hostname)
			}
		}
		// Add the vectors to the output object
		exists.insert(category.clone(), existing);
		unexists.insert(category, unexisting);
	}
	pb.lock().unwrap().finish();
	info!("Finished data collection");

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
