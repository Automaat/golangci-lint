use std::{
    collections::HashSet,
    env,
    ffi::{OsStr, OsString},
    fmt,
    fs::{self, File},
    io::{self, Write},
    path::{Path, PathBuf},
    process::{Command as StdCommand, ExitStatus},
    sync::{
        Arc, Mutex,
        atomic::{AtomicU8, AtomicU64, Ordering},
        mpsc,
    },
    thread,
    time::{Duration, Instant},
};

use command_group::{CommandGroup, GroupChild};
use serde::Serialize;
use sysinfo::{Pid, ProcessRefreshKind, ProcessesToUpdate, System};
#[cfg(windows)]
use windows_sys::Win32::System::Threading::CREATE_NEW_PROCESS_GROUP;

mod worker_transport;

pub const EXIT_SUPERVISOR: i32 = 126;
pub const EXIT_STARTUP: i32 = 127;
pub const EXIT_CANCELLED: i32 = 130;
pub const EXIT_TIMEOUT: i32 = 124;
pub const EXIT_RSS_LIMIT: i32 = 125;

const TARGET_ENV: &str = "GOLANGCI_SUPERVISOR_TARGET";
const TIMEOUT_ENV: &str = "GOLANGCI_SUPERVISOR_TIMEOUT_MS";
const MAX_RSS_ENV: &str = "GOLANGCI_SUPERVISOR_MAX_RSS_BYTES";
const REPORT_ENV: &str = "GOLANGCI_SUPERVISOR_REPORT";
const POLL_ENV: &str = "GOLANGCI_SUPERVISOR_POLL_MS";
const TRANSPORT_ENV: &str = "GOLANGCI_SUPERVISOR_TRANSPORT";
const WORKER_ENDPOINT_ENV: &str = "GOLANGCI_WORKER_ENDPOINT";
const WORKER_TOKEN_ENV: &str = "GOLANGCI_WORKER_TOKEN";
const DEFAULT_POLL_MS: u64 = 10;
const MIN_MEMORY_POLL_MS: u64 = 50;

const CAUSE_NONE: u8 = 0;
const CAUSE_CANCELLED: u8 = 1;
const CAUSE_RSS: u8 = 2;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum Transport {
    Direct,
    Worker,
}

#[derive(Debug)]
pub struct SupervisorError {
    message: String,
    exit_code: i32,
}

impl SupervisorError {
    fn new(exit_code: i32, message: impl Into<String>) -> Self {
        Self {
            message: message.into(),
            exit_code,
        }
    }

    /// Returns the stable wrapper exit code for this error.
    pub fn exit_code(&self) -> i32 {
        self.exit_code
    }
}

impl fmt::Display for SupervisorError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(&self.message)
    }
}

impl std::error::Error for SupervisorError {}

/// Supervisor-only configuration read from environment variables.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Config {
    target: OsString,
    transport: Transport,
    timeout: Option<Duration>,
    max_rss_bytes: Option<u64>,
    report: Option<PathBuf>,
    poll_interval: Duration,
}

impl Config {
    /// Reads supervisor configuration without consuming child arguments.
    pub fn from_env() -> Result<Self, SupervisorError> {
        Self::from_lookup(|name| env::var_os(name))
    }

    fn from_lookup(
        mut lookup: impl FnMut(&str) -> Option<OsString>,
    ) -> Result<Self, SupervisorError> {
        let target = lookup(TARGET_ENV)
            .filter(|value| !value.is_empty())
            .ok_or_else(|| config_error(format!("{TARGET_ENV} must name the Go executable")))?;
        let transport = match lookup(TRANSPORT_ENV) {
            None => Transport::Direct,
            Some(value) if value == "worker" => Transport::Worker,
            Some(_) => {
                return Err(config_error(format!(
                    "{TRANSPORT_ENV} must be unset or equal to worker"
                )));
            }
        };
        let timeout = parse_optional_duration(TIMEOUT_ENV, lookup(TIMEOUT_ENV))?;
        let max_rss_bytes = parse_optional_u64(MAX_RSS_ENV, lookup(MAX_RSS_ENV))?;
        let report = lookup(REPORT_ENV)
            .filter(|value| !value.is_empty())
            .map(PathBuf::from);
        let poll_ms = lookup(POLL_ENV)
            .map(|value| parse_u64(POLL_ENV, value))
            .transpose()?
            .unwrap_or(DEFAULT_POLL_MS);
        if poll_ms == 0 {
            return Err(config_error(format!(
                "{POLL_ENV} must be greater than zero"
            )));
        }

        Ok(Self {
            target,
            transport,
            timeout,
            max_rss_bytes,
            report,
            poll_interval: Duration::from_millis(poll_ms),
        })
    }
}

/// Cloneable cancellation handle shared with the OS signal listener.
#[derive(Clone, Debug)]
pub struct Cancellation {
    cause: Arc<AtomicU8>,
    requested_at: Arc<Mutex<Option<Instant>>>,
}

impl Cancellation {
    /// Creates an unset cancellation handle.
    pub fn new() -> Self {
        Self {
            cause: Arc::new(AtomicU8::new(CAUSE_NONE)),
            requested_at: Arc::new(Mutex::new(None)),
        }
    }

    /// Installs handlers for interactive and termination signals.
    pub fn install_signal_handler(&self) -> Result<(), SupervisorError> {
        let cancellation = self.clone();
        ctrlc::set_handler(move || cancellation.cancel()).map_err(|err| {
            SupervisorError::new(EXIT_SUPERVISOR, format!("install signal handler: {err}"))
        })
    }

    /// Requests cancellation from an in-process caller.
    pub fn cancel(&self) {
        self.cancel_with(CAUSE_CANCELLED);
    }

    fn cancel_for_rss(&self) {
        self.cancel_with(CAUSE_RSS);
    }

    fn cancel_with(&self, cause: u8) {
        let requested = Instant::now();
        let mut requested_at = self.requested_at.lock().expect("cancellation timestamp");
        if self
            .cause
            .compare_exchange(CAUSE_NONE, cause, Ordering::AcqRel, Ordering::Acquire)
            .is_ok()
        {
            *requested_at = Some(requested);
        }
    }

    fn cause(&self) -> u8 {
        self.cause.load(Ordering::Acquire)
    }

    fn latency(&self, finished: Instant) -> Option<Duration> {
        self.requested_at
            .lock()
            .expect("cancellation timestamp")
            .map(|requested| finished.saturating_duration_since(requested))
    }
}

impl Default for Cancellation {
    fn default() -> Self {
        Self::new()
    }
}

/// Runs one Go CLI process and returns the exit code the wrapper must expose.
pub fn run(
    config: Config,
    args: Vec<OsString>,
    cancellation: Cancellation,
) -> Result<i32, SupervisorError> {
    if config.transport == Transport::Worker {
        return worker_transport::run(config, args, cancellation);
    }

    run_direct(config, args, cancellation)
}

fn run_direct(
    config: Config,
    args: Vec<OsString>,
    cancellation: Cancellation,
) -> Result<i32, SupervisorError> {
    let started = Instant::now();
    if cancellation.cause() != CAUSE_NONE {
        return finish_cancelled(&config, started, cancellation, MemoryResult::empty());
    }

    let mut command = StdCommand::new(&config.target);
    command.args(args);
    for name in [
        TARGET_ENV,
        TIMEOUT_ENV,
        MAX_RSS_ENV,
        REPORT_ENV,
        POLL_ENV,
        TRANSPORT_ENV,
        WORKER_ENDPOINT_ENV,
        WORKER_TOKEN_ENV,
    ] {
        command.env_remove(name);
    }
    let child = match spawn_group(&mut command) {
        Ok(child) => child,
        Err(err) => {
            let report = Report::failed("startup_error", started.elapsed(), err.to_string());
            write_optional_report(config.report.as_deref(), &report)?;
            return Err(SupervisorError::new(EXIT_STARTUP, err.to_string()));
        }
    };

    let mut child = ManagedChild::new(child);
    let monitor = MemoryMonitor::start(child.id(), &config, cancellation.clone());
    let completion = loop {
        match child.try_wait() {
            Ok(Some(status)) => break Completion::Exited(status),
            Ok(None) => {}
            Err(err) => break Completion::Failed(err),
        }
        match cancellation.cause() {
            CAUSE_RSS => break Completion::RssLimit,
            CAUSE_CANCELLED => break Completion::Cancelled,
            _ => {}
        }
        let elapsed = started.elapsed();
        if config.timeout.is_some_and(|timeout| elapsed >= timeout) {
            break Completion::Timeout;
        }
        let sleep = config
            .timeout
            .map(|timeout| config.poll_interval.min(timeout.saturating_sub(elapsed)))
            .unwrap_or(config.poll_interval);
        if sleep.is_zero() {
            thread::yield_now();
        } else {
            thread::sleep(sleep);
        }
    };

    match completion {
        Completion::Exited(status) => {
            child.finish_after_exit()?;
            let memory = stop_monitor(monitor)?;
            finish_status(&config, started, status, memory)
        }
        Completion::Cancelled | Completion::RssLimit | Completion::Timeout => {
            child.terminate()?;
            let memory = stop_monitor(monitor)?;
            match completion {
                Completion::Timeout => finish_timeout(&config, started, memory),
                _ => finish_cancelled(&config, started, cancellation, memory),
            }
        }
        Completion::Failed(err) => {
            let terminate_error = child.terminate().err();
            let memory = stop_monitor(monitor).unwrap_or_else(|_| MemoryResult::unknown());
            let message = terminate_error.map_or_else(
                || err.to_string(),
                |cleanup| format!("{err}; process-tree cleanup failed: {cleanup}"),
            );
            let report = Report::lifecycle_error(started.elapsed(), memory, message.clone());
            write_optional_report(config.report.as_deref(), &report)?;
            Err(SupervisorError::new(EXIT_SUPERVISOR, message))
        }
    }
}

#[cfg(windows)]
fn spawn_group(command: &mut StdCommand) -> io::Result<GroupChild> {
    command
        .group()
        .kill_on_drop(true)
        .creation_flags(CREATE_NEW_PROCESS_GROUP)
        .spawn()
}

#[cfg(not(windows))]
fn spawn_group(command: &mut StdCommand) -> io::Result<GroupChild> {
    command.group_spawn()
}

enum Completion {
    Exited(ExitStatus),
    Cancelled,
    Timeout,
    RssLimit,
    Failed(io::Error),
}

struct ManagedChild {
    child: GroupChild,
    armed: bool,
}

impl ManagedChild {
    fn new(child: GroupChild) -> Self {
        Self { child, armed: true }
    }

    fn id(&self) -> u32 {
        self.child.id()
    }

    fn try_wait(&mut self) -> io::Result<Option<ExitStatus>> {
        self.child.try_wait()
    }

    fn finish_after_exit(&mut self) -> Result<(), SupervisorError> {
        if let Err(err) = self.child.kill()
            && !is_absent_process_error(&err)
        {
            return Err(lifecycle_error("clean up completed process group", err));
        }
        self.armed = false;
        Ok(())
    }

    fn terminate(&mut self) -> Result<(), SupervisorError> {
        let kill_error = self
            .child
            .kill()
            .err()
            .filter(|err| !is_absent_process_error(err));
        let wait_error = self.child.wait().err();
        if kill_error.is_none() && wait_error.is_none() {
            self.armed = false;
            return Ok(());
        }
        let message = match (kill_error, wait_error) {
            (Some(kill), Some(wait)) => format!("kill process group: {kill}; reap leader: {wait}"),
            (Some(kill), None) => format!("kill process group: {kill}"),
            (None, Some(wait)) => format!("reap leader: {wait}"),
            (None, None) => unreachable!(),
        };
        Err(SupervisorError::new(EXIT_SUPERVISOR, message))
    }
}

impl Drop for ManagedChild {
    fn drop(&mut self) {
        if self.armed {
            let _ = self.child.kill();
            let _ = self.child.wait();
        }
    }
}

fn finish_status(
    config: &Config,
    started: Instant,
    status: ExitStatus,
    memory: MemoryResult,
) -> Result<i32, SupervisorError> {
    let signal = exit_signal(&status);
    let (termination, wrapper_exit) = match status.code() {
        Some(code) => ("exited", code),
        None => ("signalled", signal.map_or(1, |value| 128 + value)),
    };
    let report = Report::completed(
        termination,
        status.code(),
        signal,
        started.elapsed(),
        memory,
        None,
    );
    write_optional_report(config.report.as_deref(), &report)?;
    Ok(wrapper_exit)
}

fn finish_timeout(
    config: &Config,
    started: Instant,
    memory: MemoryResult,
) -> Result<i32, SupervisorError> {
    let report = Report::completed("timeout", None, None, started.elapsed(), memory, None);
    write_optional_report(config.report.as_deref(), &report)?;
    Ok(EXIT_TIMEOUT)
}

fn finish_cancelled(
    config: &Config,
    started: Instant,
    cancellation: Cancellation,
    memory: MemoryResult,
) -> Result<i32, SupervisorError> {
    let finished = Instant::now();
    let (termination, exit_code) = match cancellation.cause() {
        CAUSE_RSS => ("rss_limit", EXIT_RSS_LIMIT),
        _ => ("cancelled", EXIT_CANCELLED),
    };
    let report = Report::completed(
        termination,
        None,
        None,
        started.elapsed(),
        memory,
        cancellation.latency(finished),
    );
    write_optional_report(config.report.as_deref(), &report)?;
    Ok(exit_code)
}

#[cfg(unix)]
fn exit_signal(status: &ExitStatus) -> Option<i32> {
    use std::os::unix::process::ExitStatusExt;

    status.signal()
}

#[cfg(not(unix))]
fn exit_signal(_status: &ExitStatus) -> Option<i32> {
    None
}

fn lifecycle_error(action: &str, err: io::Error) -> SupervisorError {
    SupervisorError::new(EXIT_SUPERVISOR, format!("{action}: {err}"))
}

fn is_absent_process_error(err: &io::Error) -> bool {
    if matches!(
        err.kind(),
        io::ErrorKind::InvalidInput | io::ErrorKind::NotFound
    ) {
        return true;
    }
    #[cfg(unix)]
    {
        err.raw_os_error() == Some(3)
    }
    #[cfg(not(unix))]
    {
        false
    }
}

struct MemoryMonitor {
    stop: mpsc::Sender<()>,
    peak: Arc<AtomicU64>,
    thread: thread::JoinHandle<()>,
}

impl MemoryMonitor {
    fn start(pid: u32, config: &Config, cancellation: Cancellation) -> Option<Self> {
        if config.report.is_none() && config.max_rss_bytes.is_none() {
            return None;
        }

        let (stop, stop_rx) = mpsc::channel();
        let peak = Arc::new(AtomicU64::new(0));
        let thread_peak = peak.clone();
        let interval = config
            .poll_interval
            .max(Duration::from_millis(MIN_MEMORY_POLL_MS));
        let limit = config.max_rss_bytes;
        let thread = thread::spawn(move || {
            let mut sampler = ProcessTreeSampler::new(pid);
            loop {
                let rss = sampler.sample();
                thread_peak.fetch_max(rss, Ordering::AcqRel);
                if limit.is_some_and(|max| rss > max) {
                    cancellation.cancel_for_rss();
                    return;
                }
                if stop_rx.recv_timeout(interval).is_ok() {
                    return;
                }
            }
        });

        Some(Self { stop, peak, thread })
    }

    fn stop(self) -> Result<MemoryResult, SupervisorError> {
        let _ = self.stop.send(());
        self.thread
            .join()
            .map_err(|_| SupervisorError::new(EXIT_SUPERVISOR, "memory monitor panicked"))?;
        Ok(MemoryResult {
            peak_rss: self.peak.load(Ordering::Acquire),
            survivors: Some(0),
        })
    }
}

fn stop_monitor(monitor: Option<MemoryMonitor>) -> Result<MemoryResult, SupervisorError> {
    match monitor {
        Some(monitor) => monitor.stop(),
        None => Ok(MemoryResult::empty()),
    }
}

#[derive(Clone, Copy)]
struct MemoryResult {
    peak_rss: u64,
    survivors: Option<usize>,
}

impl MemoryResult {
    fn empty() -> Self {
        Self {
            peak_rss: 0,
            survivors: Some(0),
        }
    }

    fn unknown() -> Self {
        Self {
            peak_rss: 0,
            survivors: None,
        }
    }
}

struct ProcessTreeSampler {
    root: Pid,
    system: System,
}

impl ProcessTreeSampler {
    fn new(root: u32) -> Self {
        Self {
            root: Pid::from_u32(root),
            system: System::new(),
        }
    }

    fn sample(&mut self) -> u64 {
        refresh_processes(&mut self.system);
        let mut members = HashSet::from([self.root]);
        loop {
            let before = members.len();
            for (pid, process) in self.system.processes() {
                if process
                    .parent()
                    .is_some_and(|parent| members.contains(&parent))
                {
                    members.insert(*pid);
                }
            }
            if members.len() == before {
                break;
            }
        }
        members
            .iter()
            .filter_map(|pid| self.system.process(*pid))
            .map(sysinfo::Process::memory)
            .sum()
    }
}

fn refresh_processes(system: &mut System) {
    system.refresh_processes_specifics(
        ProcessesToUpdate::All,
        true,
        ProcessRefreshKind::nothing().with_memory().without_tasks(),
    );
}

#[derive(Debug, Serialize)]
struct Report {
    schema_version: u8,
    termination: &'static str,
    child_exit_code: Option<i32>,
    child_signal: Option<i32>,
    elapsed_ms: u64,
    peak_tree_rss_bytes: u64,
    cancellation_latency_ms: Option<u64>,
    surviving_descendants: Option<usize>,
    error: Option<String>,
}

impl Report {
    fn completed(
        termination: &'static str,
        child_exit_code: Option<i32>,
        child_signal: Option<i32>,
        elapsed: Duration,
        memory: MemoryResult,
        cancellation_latency: Option<Duration>,
    ) -> Self {
        Self {
            schema_version: 1,
            termination,
            child_exit_code,
            child_signal,
            elapsed_ms: millis(elapsed),
            peak_tree_rss_bytes: memory.peak_rss,
            cancellation_latency_ms: cancellation_latency.map(millis),
            surviving_descendants: memory.survivors,
            error: None,
        }
    }

    fn failed(termination: &'static str, elapsed: Duration, error: String) -> Self {
        Self {
            schema_version: 1,
            termination,
            child_exit_code: None,
            child_signal: None,
            elapsed_ms: millis(elapsed),
            peak_tree_rss_bytes: 0,
            cancellation_latency_ms: None,
            surviving_descendants: Some(0),
            error: Some(error),
        }
    }

    fn lifecycle_error(elapsed: Duration, memory: MemoryResult, error: String) -> Self {
        let mut report = Self::failed("lifecycle_error", elapsed, error);
        report.peak_tree_rss_bytes = memory.peak_rss;
        report.surviving_descendants = memory.survivors;
        report
    }
}

fn write_optional_report(path: Option<&Path>, report: &Report) -> Result<(), SupervisorError> {
    let Some(path) = path else {
        return Ok(());
    };
    write_report(path, report).map_err(|err| {
        SupervisorError::new(
            EXIT_SUPERVISOR,
            format!("write outcome report {}: {err}", path.display()),
        )
    })
}

fn write_report(path: &Path, report: &Report) -> io::Result<()> {
    let file_name = path
        .file_name()
        .unwrap_or_else(|| OsStr::new("outcome.json"));
    let temp = path.with_file_name(format!(
        ".{}.{}.tmp",
        file_name.to_string_lossy(),
        std::process::id()
    ));
    let mut file = File::create(&temp)?;
    serde_json::to_writer(&mut file, report)?;
    file.write_all(b"\n")?;
    if cfg!(windows) && path.exists() {
        fs::remove_file(path)?;
    }
    fs::rename(temp, path)
}

fn parse_optional_duration(
    name: &str,
    value: Option<OsString>,
) -> Result<Option<Duration>, SupervisorError> {
    Ok(parse_optional_u64(name, value)?.map(Duration::from_millis))
}

fn parse_optional_u64(name: &str, value: Option<OsString>) -> Result<Option<u64>, SupervisorError> {
    let Some(value) = value else {
        return Ok(None);
    };
    let parsed = parse_u64(name, value)?;
    Ok((parsed != 0).then_some(parsed))
}

fn parse_u64(name: &str, value: OsString) -> Result<u64, SupervisorError> {
    let value = value
        .into_string()
        .map_err(|_| config_error(format!("{name} must be valid UTF-8")))?;
    value
        .parse::<u64>()
        .map_err(|_| config_error(format!("{name} must be an unsigned integer")))
}

fn config_error(message: impl Into<String>) -> SupervisorError {
    SupervisorError::new(EXIT_SUPERVISOR, message)
}

fn millis(duration: Duration) -> u64 {
    duration.as_millis().min(u128::from(u64::MAX)) as u64
}

#[cfg(test)]
mod tests {
    use std::{collections::HashMap, ffi::OsString, time::Duration};

    use super::{
        Config, MAX_RSS_ENV, MemoryResult, POLL_ENV, REPORT_ENV, Report, TARGET_ENV, TIMEOUT_ENV,
        TRANSPORT_ENV, Transport, millis,
    };

    #[test]
    fn parses_configuration() {
        let values = HashMap::from([
            (TARGET_ENV, OsString::from("go-lint")),
            (TIMEOUT_ENV, OsString::from("250")),
            (MAX_RSS_ENV, OsString::from("4096")),
            (REPORT_ENV, OsString::from("outcome.json")),
            (POLL_ENV, OsString::from("20")),
            (TRANSPORT_ENV, OsString::from("worker")),
        ]);
        let config = Config::from_lookup(|name| values.get(name).cloned()).unwrap();

        assert_eq!(config.target, OsString::from("go-lint"));
        assert_eq!(config.transport, Transport::Worker);
        assert_eq!(config.timeout, Some(Duration::from_millis(250)));
        assert_eq!(config.max_rss_bytes, Some(4096));
        assert_eq!(config.report.unwrap().to_string_lossy(), "outcome.json");
        assert_eq!(config.poll_interval, Duration::from_millis(20));
    }

    #[test]
    fn zero_disables_optional_limits() {
        let values = HashMap::from([
            (TARGET_ENV, OsString::from("go-lint")),
            (TIMEOUT_ENV, OsString::from("0")),
            (MAX_RSS_ENV, OsString::from("0")),
        ]);
        let config = Config::from_lookup(|name| values.get(name).cloned()).unwrap();

        assert_eq!(config.timeout, None);
        assert_eq!(config.max_rss_bytes, None);
    }

    #[test]
    fn rejects_missing_target_and_zero_poll() {
        assert!(Config::from_lookup(|_| None).is_err());
        let values = HashMap::from([
            (TARGET_ENV, OsString::from("go-lint")),
            (POLL_ENV, OsString::from("0")),
        ]);
        assert!(Config::from_lookup(|name| values.get(name).cloned()).is_err());

        let values = HashMap::from([
            (TARGET_ENV, OsString::from("go-lint")),
            (TRANSPORT_ENV, OsString::from("stdio")),
        ]);
        assert!(Config::from_lookup(|name| values.get(name).cloned()).is_err());
    }

    #[test]
    fn duration_conversion_saturates() {
        assert_eq!(millis(Duration::MAX), u64::MAX);
    }

    #[test]
    fn outcome_report_is_versioned() {
        let report = Report::completed(
            "timeout",
            None,
            None,
            Duration::from_millis(42),
            MemoryResult {
                peak_rss: 1024,
                survivors: Some(0),
            },
            Some(Duration::from_millis(3)),
        );
        let value = serde_json::to_value(report).unwrap();

        assert_eq!(value["schema_version"], 1);
        assert_eq!(value["termination"], "timeout");
        assert_eq!(value["elapsed_ms"], 42);
        assert_eq!(value["peak_tree_rss_bytes"], 1024);
        assert_eq!(value["cancellation_latency_ms"], 3);
        assert_eq!(value["surviving_descendants"], 0);
    }
}
