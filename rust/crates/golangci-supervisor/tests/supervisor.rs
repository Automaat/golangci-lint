use std::{
    fs,
    path::{Path, PathBuf},
    process::{Child, Command, ExitStatus, Stdio},
    sync::atomic::{AtomicU64, Ordering},
    thread,
    time::{Duration, Instant},
};

use serde_json::Value;
#[cfg(windows)]
use std::os::windows::process::CommandExt;
#[cfg(windows)]
use windows_sys::Win32::System::{
    Console::{CTRL_BREAK_EVENT, GenerateConsoleCtrlEvent},
    Threading::CREATE_NEW_PROCESS_GROUP,
};

const TARGET_ENV: &str = "GOLANGCI_SUPERVISOR_TARGET";
const TIMEOUT_ENV: &str = "GOLANGCI_SUPERVISOR_TIMEOUT_MS";
const MAX_RSS_ENV: &str = "GOLANGCI_SUPERVISOR_MAX_RSS_BYTES";
const REPORT_ENV: &str = "GOLANGCI_SUPERVISOR_REPORT";
const POLL_ENV: &str = "GOLANGCI_SUPERVISOR_POLL_MS";
const SUPERVISOR: &str = env!("CARGO_BIN_EXE_golangci-supervisor");
const FIXTURE: &str = env!("CARGO_BIN_EXE_supervisor-fixture");

static NEXT_TEMP: AtomicU64 = AtomicU64::new(0);

#[test]
fn preserves_arguments_streams_and_exit_code() {
    let report = temp_path("passthrough-report");
    let output = supervisor(&report)
        .args(["echo", "hello world", "7"])
        .output()
        .unwrap();

    assert_eq!(output.status.code(), Some(7));
    assert_eq!(
        String::from_utf8(output.stdout).unwrap(),
        "stdout:hello world\n"
    );
    assert_eq!(
        String::from_utf8(output.stderr).unwrap(),
        "stderr:fixture\n"
    );
    let report = read_report(&report);
    assert_eq!(report["termination"], "exited");
    assert_eq!(report["child_exit_code"], 7);
    assert_eq!(report["surviving_descendants"], 0);
}

#[test]
fn timeout_kills_descendants() {
    let report_path = temp_path("timeout-report");
    let pid_path = temp_path("timeout-pid");
    let mut child = supervisor(&report_path)
        .env(TIMEOUT_ENV, "1000")
        .args(["spawn-descendant", path_string(&pid_path), "5000"])
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .spawn()
        .unwrap();

    wait_for_file(&pid_path, Duration::from_secs(3));
    let status = wait_for_exit(&mut child, Duration::from_secs(5));
    assert_eq!(status.code(), Some(124));
    let descendant = read_pid(&pid_path);
    assert_process_gone(descendant);
    let report = read_report(&report_path);
    assert_eq!(report["termination"], "timeout");
    assert_eq!(report["surviving_descendants"], 0);
}

#[test]
fn timeout_is_not_delayed_by_poll_interval() {
    let report_path = temp_path("timeout-poll-report");
    let output = supervisor(&report_path)
        .env(TIMEOUT_ENV, "50")
        .env(POLL_ENV, "1000")
        .args(["sleep", "5000"])
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .output()
        .unwrap();

    assert_eq!(output.status.code(), Some(124));
    let report = read_report(&report_path);
    assert_eq!(report["termination"], "timeout");
    assert!(report["elapsed_ms"].as_u64().unwrap() < 500);
}

#[test]
fn rss_limit_kills_process_tree() {
    let report_path = temp_path("rss-report");
    let mut child = supervisor(&report_path)
        .env(TIMEOUT_ENV, "3000")
        .env(MAX_RSS_ENV, (16 * 1024 * 1024).to_string())
        .args(["allocate", "64", "5000"])
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .spawn()
        .unwrap();

    let status = wait_for_exit(&mut child, Duration::from_secs(5));
    assert_eq!(status.code(), Some(125));
    let report = read_report(&report_path);
    assert_eq!(report["termination"], "rss_limit");
    assert!(report["peak_tree_rss_bytes"].as_u64().unwrap() > 16 * 1024 * 1024);
    assert_eq!(report["surviving_descendants"], 0);
}

#[test]
fn interrupt_cancels_and_kills_descendants() {
    let report_path = temp_path("cancel-report");
    let pid_path = temp_path("cancel-pid");
    let mut command = supervisor(&report_path);
    command
        .env(TIMEOUT_ENV, "3000")
        .args(["spawn-descendant", path_string(&pid_path), "5000"])
        .stdout(Stdio::null())
        .stderr(Stdio::null());
    #[cfg(windows)]
    command.creation_flags(CREATE_NEW_PROCESS_GROUP);
    let mut child = command.spawn().unwrap();

    wait_for_file(&pid_path, Duration::from_secs(3));
    request_interrupt(child.id());
    let status = wait_for_exit(&mut child, Duration::from_secs(5));
    assert_eq!(status.code(), Some(130));
    assert_process_gone(read_pid(&pid_path));
    let report = read_report(&report_path);
    assert_eq!(report["termination"], "cancelled");
    assert!(report["cancellation_latency_ms"].as_u64().unwrap() < 5_000);
    assert_eq!(report["surviving_descendants"], 0);
}

#[cfg(unix)]
fn request_interrupt(pid: u32) {
    let status = Command::new("kill")
        .args(["-INT", &pid.to_string()])
        .status()
        .unwrap();
    assert!(status.success());
}

#[cfg(windows)]
fn request_interrupt(pid: u32) {
    // The supervisor is spawned into this dedicated console process group.
    let sent = unsafe { GenerateConsoleCtrlEvent(CTRL_BREAK_EVENT, pid) };
    assert_ne!(sent, 0);
}

fn supervisor(report: &Path) -> Command {
    let mut command = Command::new(SUPERVISOR);
    command
        .env(TARGET_ENV, FIXTURE)
        .env(REPORT_ENV, report)
        .env(POLL_ENV, "5")
        .env_remove(TIMEOUT_ENV)
        .env_remove(MAX_RSS_ENV);
    command
}

fn wait_for_exit(child: &mut Child, timeout: Duration) -> ExitStatus {
    let deadline = Instant::now() + timeout;
    loop {
        if let Some(status) = child.try_wait().unwrap() {
            return status;
        }
        if Instant::now() >= deadline {
            child.kill().unwrap();
            panic!("supervisor did not exit within {timeout:?}");
        }
        thread::sleep(Duration::from_millis(10));
    }
}

fn wait_for_file(path: &Path, timeout: Duration) {
    let deadline = Instant::now() + timeout;
    while !path.exists() {
        assert!(
            Instant::now() < deadline,
            "fixture did not write {}",
            path.display()
        );
        thread::sleep(Duration::from_millis(10));
    }
}

fn read_report(path: &Path) -> Value {
    serde_json::from_slice(&fs::read(path).unwrap()).unwrap()
}

fn read_pid(path: &Path) -> u32 {
    fs::read_to_string(path).unwrap().parse().unwrap()
}

fn temp_path(label: &str) -> PathBuf {
    let sequence = NEXT_TEMP.fetch_add(1, Ordering::Relaxed);
    std::env::temp_dir().join(format!(
        "golangci-supervisor-{label}-{}-{sequence}",
        std::process::id()
    ))
}

fn path_string(path: &Path) -> &str {
    path.to_str().unwrap()
}

#[cfg(unix)]
fn assert_process_gone(pid: u32) {
    let status = Command::new("kill")
        .args(["-0", &pid.to_string()])
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .status()
        .unwrap();
    assert!(!status.success(), "process {pid} survived");
}

#[cfg(windows)]
fn assert_process_gone(pid: u32) {
    let output = Command::new("tasklist")
        .args(["/FI", &format!("PID eq {pid}"), "/FO", "CSV", "/NH"])
        .output()
        .unwrap();
    let stdout = String::from_utf8_lossy(&output.stdout);
    assert!(
        !stdout.contains(&format!("\"{pid}\"")),
        "process {pid} survived"
    );
}
