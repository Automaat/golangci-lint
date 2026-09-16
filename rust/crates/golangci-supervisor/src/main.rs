use std::{env, process};

use golangci_supervisor::{Cancellation, Config, EXIT_SUPERVISOR};

fn main() {
    let config = match Config::from_env() {
        Ok(config) => config,
        Err(err) => {
            eprintln!("golangci-supervisor: {err}");
            process::exit(EXIT_SUPERVISOR);
        }
    };

    let cancellation = Cancellation::new();
    if let Err(err) = cancellation.install_signal_handler() {
        eprintln!("golangci-supervisor: {err}");
        process::exit(EXIT_SUPERVISOR);
    }

    let args = env::args_os().skip(1).collect();
    match golangci_supervisor::run(config, args, cancellation) {
        Ok(exit_code) => process::exit(exit_code),
        Err(err) => {
            eprintln!("golangci-supervisor: {err}");
            process::exit(err.exit_code());
        }
    }
}
