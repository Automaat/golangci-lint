use std::{
    collections::BTreeMap,
    ffi::OsString,
    io::{self, BufRead, BufReader, Read, Write},
    net::{TcpListener, TcpStream},
    process::ExitStatus,
    sync::mpsc::{self, Receiver, TryRecvError},
    thread,
    time::{Duration, Instant},
};

use golangci_protocol::{
    CompletePayload, Decoder, Envelope, ErrorPayload, HelloPayload, Kind, LifecyclePayload,
    MAX_LINE_BYTES, RunPayload, ShutdownPayload, decode_payload, encode,
};
#[cfg(unix)]
use std::fs::File;
#[cfg(windows)]
use windows_sys::Win32::Security::Cryptography::{
    BCRYPT_USE_SYSTEM_PREFERRED_RNG, BCryptGenRandom,
};

use super::{
    CAUSE_CANCELLED, CAUSE_NONE, CAUSE_RSS, Cancellation, Config, EXIT_SUPERVISOR, MAX_RSS_ENV,
    ManagedChild, MemoryMonitor, MemoryResult, POLL_ENV, REPORT_ENV, Report, SupervisorError,
    TARGET_ENV, TIMEOUT_ENV, TRANSPORT_ENV, WORKER_ENDPOINT_ENV, WORKER_TOKEN_ENV,
    finish_cancelled, finish_timeout, spawn_group, stop_monitor, write_optional_report,
};

const REQUEST_ID: &str = "run-1";
const HANDSHAKE_TIMEOUT: Duration = Duration::from_secs(5);
const AUTH_CANDIDATE_TIMEOUT: Duration = Duration::from_secs(1);
const AUTH_POLL_INTERVAL: Duration = Duration::from_millis(50);
const SESSION_WRITE_TIMEOUT: Duration = Duration::from_millis(250);
const SHUTDOWN_TIMEOUT: Duration = Duration::from_secs(1);

enum WorkerSignal {
    Ready,
    Complete(CompletePayload),
    ShutdownAcknowledged,
}

enum WorkerCommand {
    Run(RunPayload),
    Shutdown,
}

struct WorkerSession {
    commands: mpsc::Sender<WorkerCommand>,
    reader_thread: thread::JoinHandle<()>,
    writer_thread: thread::JoinHandle<()>,
}

impl WorkerSession {
    fn send(&self, command: WorkerCommand) -> Result<(), SupervisorError> {
        self.commands
            .send(command)
            .map_err(|_| transport_error("worker transport writer stopped"))
    }

    fn finish(self) {
        drop(self.commands);
        let _ = self.reader_thread.join();
        let _ = self.writer_thread.join();
    }
}

#[derive(serde::Deserialize)]
struct WorkerReadyPayload {
    worker: String,
    capabilities: Vec<String>,
    auth_token: String,
}

pub(super) fn run(
    config: Config,
    args: Vec<OsString>,
    cancellation: Cancellation,
) -> Result<i32, SupervisorError> {
    let started = Instant::now();
    if cancellation.cause() != CAUSE_NONE {
        return finish_cancelled(&config, started, cancellation, MemoryResult::empty());
    }

    let run = build_run_payload(args)?;
    let listener = TcpListener::bind(("127.0.0.1", 0))
        .map_err(|error| transport_error(format!("bind worker transport: {error}")))?;
    listener
        .set_nonblocking(true)
        .map_err(|error| transport_error(format!("configure worker transport: {error}")))?;
    let endpoint = listener
        .local_addr()
        .map_err(|error| transport_error(format!("read worker endpoint: {error}")))?;
    let worker_token = generate_worker_token()?;

    let mut command = std::process::Command::new(&config.target);
    command
        .env(WORKER_ENDPOINT_ENV, endpoint.to_string())
        .env(WORKER_TOKEN_ENV, &worker_token);
    for name in [
        TARGET_ENV,
        TIMEOUT_ENV,
        MAX_RSS_ENV,
        REPORT_ENV,
        POLL_ENV,
        TRANSPORT_ENV,
    ] {
        command.env_remove(name);
    }
    let child = match spawn_group(&mut command) {
        Ok(child) => child,
        Err(error) => {
            let message = format!(
                "start Go worker {}: {error}",
                config.target.to_string_lossy()
            );
            let report = Report::failed("startup_error", started.elapsed(), message.clone());
            write_optional_report(config.report.as_deref(), &report)?;
            return Err(SupervisorError::new(super::EXIT_STARTUP, message));
        }
    };
    let mut child = ManagedChild::new(child);
    let monitor = MemoryMonitor::start(child.id(), &config, cancellation.clone());

    let authenticated = match accept_worker(
        &listener,
        &config,
        started,
        &mut child,
        &cancellation,
        &worker_token,
    ) {
        Ok(authenticated) => authenticated,
        Err(AcceptFailure::Control(control)) => {
            child.terminate()?;
            let memory = stop_monitor(monitor)?;
            return finish_control(&config, started, cancellation, memory, control);
        }
        Err(AcceptFailure::Exited(status)) => {
            child.finish_after_exit()?;
            let memory = stop_monitor(monitor)?;
            return finish_protocol_failure(
                &config,
                started,
                memory,
                format!("Go worker exited before connecting: {status}"),
            );
        }
        Err(AcceptFailure::Protocol(error)) => {
            let terminate = child.terminate().err();
            let memory = stop_monitor(monitor).unwrap_or_else(|_| MemoryResult::unknown());
            let message = append_cleanup_error(error.to_string(), terminate);
            return finish_protocol_failure(&config, started, memory, message);
        }
    };

    let writer = match authenticated.reader.get_ref().try_clone() {
        Ok(writer) => writer,
        Err(error) => {
            let terminate = child.terminate().err();
            let memory = stop_monitor(monitor).unwrap_or_else(|_| MemoryResult::unknown());
            return finish_protocol_failure(
                &config,
                started,
                memory,
                append_cleanup_error(format!("clone worker transport: {error}"), terminate),
            );
        }
    };
    let (signals, session) = start_session(authenticated.reader, authenticated.decoder, writer);

    supervise_session(
        config,
        started,
        cancellation,
        child,
        monitor,
        signals,
        session,
        run,
    )
}

#[allow(clippy::too_many_arguments)]
fn supervise_session(
    config: Config,
    started: Instant,
    cancellation: Cancellation,
    mut child: ManagedChild,
    monitor: Option<MemoryMonitor>,
    signals: Receiver<Result<WorkerSignal, SupervisorError>>,
    session: WorkerSession,
    run: RunPayload,
) -> Result<i32, SupervisorError> {
    let handshake_deadline = Instant::now() + HANDSHAKE_TIMEOUT;
    let mut ready = false;
    let mut complete = None;
    let mut shutdown_acknowledged = false;
    let mut shutdown_deadline = None;
    let mut run = Some(run);

    loop {
        match signals.try_recv() {
            Ok(Ok(WorkerSignal::Ready)) if !ready => {
                ready = true;
                if let Err(error) =
                    session.send(WorkerCommand::Run(run.take().expect("run is sent once")))
                {
                    return fail_session(
                        &config,
                        started,
                        child,
                        monitor,
                        session,
                        error.to_string(),
                    );
                }
            }
            Ok(Ok(WorkerSignal::Ready)) => {
                return fail_session(
                    &config,
                    started,
                    child,
                    monitor,
                    session,
                    "Go worker sent ready twice".to_owned(),
                );
            }
            Ok(Ok(WorkerSignal::Complete(payload))) if ready && complete.is_none() => {
                if let Err(error) = session.send(WorkerCommand::Shutdown) {
                    return fail_session(
                        &config,
                        started,
                        child,
                        monitor,
                        session,
                        error.to_string(),
                    );
                }
                complete = Some(payload);
                shutdown_deadline = Some(Instant::now() + SHUTDOWN_TIMEOUT);
            }
            Ok(Ok(WorkerSignal::Complete(_))) => {
                return fail_session(
                    &config,
                    started,
                    child,
                    monitor,
                    session,
                    "Go worker completed before ready or completed twice".to_owned(),
                );
            }
            Ok(Ok(WorkerSignal::ShutdownAcknowledged))
                if complete.is_some() && !shutdown_acknowledged =>
            {
                shutdown_acknowledged = true;
            }
            Ok(Ok(WorkerSignal::ShutdownAcknowledged)) => {
                return fail_session(
                    &config,
                    started,
                    child,
                    monitor,
                    session,
                    "Go worker acknowledged shutdown out of order".to_owned(),
                );
            }
            Ok(Err(error)) => {
                return fail_session(&config, started, child, monitor, session, error.to_string());
            }
            Err(TryRecvError::Disconnected) if complete.is_none() => {
                return fail_session(
                    &config,
                    started,
                    child,
                    monitor,
                    session,
                    "Go worker closed the protocol without completion".to_owned(),
                );
            }
            Err(TryRecvError::Empty | TryRecvError::Disconnected) => {}
        }

        match child.try_wait() {
            Ok(Some(status)) => {
                child.finish_after_exit()?;
                let memory = stop_monitor(monitor)?;
                session.finish();
                while let Ok(signal) = signals.try_recv() {
                    match signal {
                        Ok(WorkerSignal::Complete(payload)) if complete.is_none() => {
                            complete = Some(payload);
                        }
                        Ok(WorkerSignal::ShutdownAcknowledged) if !shutdown_acknowledged => {
                            shutdown_acknowledged = true;
                        }
                        Err(error) => {
                            return finish_protocol_failure(
                                &config,
                                started,
                                memory,
                                error.to_string(),
                            );
                        }
                        _ => {}
                    }
                }
                if complete.is_some() && !shutdown_acknowledged {
                    return finish_protocol_failure(
                        &config,
                        started,
                        memory,
                        "Go worker exited without acknowledging shutdown".to_owned(),
                    );
                }
                return finish_worker_status(&config, started, memory, status, complete);
            }
            Ok(None) => {}
            Err(error) => {
                return fail_session(
                    &config,
                    started,
                    child,
                    monitor,
                    session,
                    format!("wait for Go worker: {error}"),
                );
            }
        }

        let control = control_state(&config, started, &cancellation);
        if control != Control::Continue {
            child.terminate()?;
            let memory = stop_monitor(monitor)?;
            session.finish();
            return finish_control(&config, started, cancellation, memory, control);
        }
        if !ready && Instant::now() >= handshake_deadline {
            return fail_session(
                &config,
                started,
                child,
                monitor,
                session,
                "Go worker handshake timed out".to_owned(),
            );
        }
        if shutdown_deadline.is_some_and(|deadline| Instant::now() >= deadline) {
            return fail_session(
                &config,
                started,
                child,
                monitor,
                session,
                "Go worker shutdown timed out".to_owned(),
            );
        }

        let mut sleep = config.poll_interval;
        if !ready {
            sleep = sleep.min(handshake_deadline.saturating_duration_since(Instant::now()));
        }
        if let Some(deadline) = shutdown_deadline {
            sleep = sleep.min(deadline.saturating_duration_since(Instant::now()));
        }
        if sleep.is_zero() {
            thread::yield_now();
        } else {
            thread::sleep(sleep);
        }
    }
}

fn build_run_payload(args: Vec<OsString>) -> Result<RunPayload, SupervisorError> {
    let args = args
        .into_iter()
        .map(|value| {
            value
                .into_string()
                .map_err(|_| transport_error("worker arguments must be valid UTF-8"))
        })
        .collect::<Result<Vec<_>, _>>()?;
    if !args.iter().any(|arg| arg == "run") {
        return Err(transport_error(
            "worker transport only supports the run command",
        ));
    }
    let working_directory = std::env::current_dir()
        .map_err(|error| transport_error(format!("read working directory: {error}")))?
        .into_os_string()
        .into_string()
        .map_err(|_| transport_error("worker directory must be valid UTF-8"))?;

    Ok(RunPayload {
        args,
        working_directory,
        environment: BTreeMap::new(),
    })
}

enum AcceptFailure {
    Control(Control),
    Exited(ExitStatus),
    Protocol(SupervisorError),
}

enum AuthenticateFailure {
    Candidate(SupervisorError),
    Accept(AcceptFailure),
}

struct AuthenticatedWorker {
    reader: BufReader<TcpStream>,
    decoder: Decoder,
}

fn accept_worker(
    listener: &TcpListener,
    config: &Config,
    started: Instant,
    child: &mut ManagedChild,
    cancellation: &Cancellation,
    worker_token: &str,
) -> Result<AuthenticatedWorker, AcceptFailure> {
    let deadline = Instant::now() + HANDSHAKE_TIMEOUT;
    let mut candidate_error = None;
    loop {
        match listener.accept() {
            Ok((stream, address)) => {
                if !address.ip().is_loopback() {
                    candidate_error = Some(transport_error(
                        "worker transport accepted a non-loopback peer",
                    ));
                    continue;
                }
                stream.set_nonblocking(false).map_err(|error| {
                    AcceptFailure::Protocol(transport_error(format!(
                        "configure Go worker stream: {error}"
                    )))
                })?;
                let remaining = deadline.saturating_duration_since(Instant::now());
                if remaining.is_zero() {
                    return Err(AcceptFailure::Protocol(candidate_error.unwrap_or_else(
                        || transport_error("Go worker authentication timed out"),
                    )));
                }
                match authenticate_worker(
                    stream,
                    worker_token,
                    Instant::now() + AUTH_CANDIDATE_TIMEOUT.min(remaining),
                    config,
                    started,
                    child,
                    cancellation,
                ) {
                    Ok(authenticated) => return Ok(authenticated),
                    Err(AuthenticateFailure::Candidate(error)) => candidate_error = Some(error),
                    Err(AuthenticateFailure::Accept(error)) => return Err(error),
                }
            }
            Err(error) if error.kind() == io::ErrorKind::WouldBlock => {}
            Err(error) => {
                return Err(AcceptFailure::Protocol(transport_error(format!(
                    "accept Go worker: {error}"
                ))));
            }
        }
        match child.try_wait() {
            Ok(Some(status)) => return Err(AcceptFailure::Exited(status)),
            Ok(None) => {}
            Err(error) => {
                return Err(AcceptFailure::Protocol(transport_error(format!(
                    "wait for Go worker: {error}"
                ))));
            }
        }
        let control = control_state(config, started, cancellation);
        if control != Control::Continue {
            return Err(AcceptFailure::Control(control));
        }
        if Instant::now() >= deadline {
            return Err(AcceptFailure::Protocol(candidate_error.unwrap_or_else(
                || transport_error("Go worker connection timed out"),
            )));
        }
        thread::sleep(
            config
                .poll_interval
                .min(deadline.saturating_duration_since(Instant::now())),
        );
    }
}

fn authenticate_worker(
    mut stream: TcpStream,
    worker_token: &str,
    deadline: Instant,
    config: &Config,
    started: Instant,
    child: &mut ManagedChild,
    cancellation: &Cancellation,
) -> Result<AuthenticatedWorker, AuthenticateFailure> {
    let timeout = authentication_poll_timeout(config, started, deadline)?;
    stream.set_read_timeout(Some(timeout)).map_err(|error| {
        candidate_error(format!("configure worker authentication read: {error}"))
    })?;
    stream.set_write_timeout(Some(timeout)).map_err(|error| {
        candidate_error(format!("configure worker authentication write: {error}"))
    })?;
    write_envelope(
        &mut stream,
        Kind::Hello,
        None,
        None,
        &HelloPayload {
            client: "golangci-supervisor".to_owned(),
            capabilities: vec!["lifecycle".to_owned()],
        },
    )
    .map_err(AuthenticateFailure::Candidate)?;

    let mut reader = BufReader::new(stream);
    let record =
        read_authentication_record(&mut reader, config, started, child, cancellation, deadline)?;
    let mut decoder = Decoder::default();
    let envelope = decoder
        .decode_line(&record)
        .map_err(|error| candidate_error(format!("decode Go worker ready event: {error}")))?;
    if envelope.kind != Kind::Ready {
        return Err(candidate_error(format!(
            "unexpected Go worker authentication event: {:?}",
            envelope.kind
        )));
    }
    let payload: WorkerReadyPayload = decode_payload(&envelope)
        .map_err(|error| candidate_error(format!("decode Go worker ready event: {error}")))?;
    validate_worker_ready(&payload, worker_token).map_err(AuthenticateFailure::Candidate)?;

    reader
        .get_ref()
        .set_read_timeout(None)
        .map_err(|error| candidate_error(format!("clear worker authentication read: {error}")))?;
    reader
        .get_ref()
        .set_write_timeout(Some(SESSION_WRITE_TIMEOUT))
        .map_err(|error| candidate_error(format!("configure worker session write: {error}")))?;

    Ok(AuthenticatedWorker { reader, decoder })
}

fn read_authentication_record(
    reader: &mut BufReader<TcpStream>,
    config: &Config,
    started: Instant,
    child: &mut ManagedChild,
    cancellation: &Cancellation,
    deadline: Instant,
) -> Result<Vec<u8>, AuthenticateFailure> {
    let mut record = Vec::new();
    loop {
        let timeout = authentication_poll_timeout(config, started, deadline)?;
        reader
            .get_ref()
            .set_read_timeout(Some(timeout))
            .map_err(|error| {
                candidate_error(format!("configure worker authentication read: {error}"))
            })?;
        let remaining = MAX_LINE_BYTES + 2 - record.len().min(MAX_LINE_BYTES + 2);
        if remaining == 0 {
            return Err(candidate_error(
                "Go worker authentication exceeds the protocol limit",
            ));
        }
        let result = (&mut *reader)
            .take(remaining as u64)
            .read_until(b'\n', &mut record);
        match result {
            Ok(0) => {
                return Err(candidate_error(
                    "Go worker closed the protocol during authentication",
                ));
            }
            Ok(_) => {
                if record.len() > MAX_LINE_BYTES + 1 {
                    return Err(candidate_error(
                        "Go worker authentication exceeds the protocol limit",
                    ));
                }
                if record.last() != Some(&b'\n') {
                    return Err(candidate_error(
                        "Go worker authentication is not newline terminated",
                    ));
                }
                return Ok(record);
            }
            Err(error)
                if matches!(
                    error.kind(),
                    io::ErrorKind::TimedOut | io::ErrorKind::WouldBlock
                ) =>
            {
                if record.len() > MAX_LINE_BYTES + 1 {
                    return Err(candidate_error(
                        "Go worker authentication exceeds the protocol limit",
                    ));
                }
                check_authentication_state(config, started, child, cancellation, deadline)?;
            }
            Err(error) => {
                return Err(candidate_error(format!(
                    "read Go worker authentication: {error}"
                )));
            }
        }
    }
}

fn authentication_poll_timeout(
    config: &Config,
    started: Instant,
    deadline: Instant,
) -> Result<Duration, AuthenticateFailure> {
    let now = Instant::now();
    if now >= deadline {
        return Err(candidate_error("Go worker authentication timed out"));
    }
    let mut timeout = AUTH_POLL_INTERVAL.min(deadline.saturating_duration_since(now));
    if let Some(limit) = config.timeout {
        let remaining = limit.saturating_sub(started.elapsed());
        if remaining.is_zero() {
            return Err(AuthenticateFailure::Accept(AcceptFailure::Control(
                Control::Timeout,
            )));
        }
        timeout = timeout.min(remaining);
    }

    Ok(timeout)
}

fn check_authentication_state(
    config: &Config,
    started: Instant,
    child: &mut ManagedChild,
    cancellation: &Cancellation,
    deadline: Instant,
) -> Result<(), AuthenticateFailure> {
    match child.try_wait() {
        Ok(Some(status)) => {
            return Err(AuthenticateFailure::Accept(AcceptFailure::Exited(status)));
        }
        Ok(None) => {}
        Err(error) => {
            return Err(AuthenticateFailure::Accept(AcceptFailure::Protocol(
                transport_error(format!("wait for Go worker: {error}")),
            )));
        }
    }
    let control = control_state(config, started, cancellation);
    if control != Control::Continue {
        return Err(AuthenticateFailure::Accept(AcceptFailure::Control(control)));
    }
    if Instant::now() >= deadline {
        return Err(candidate_error("Go worker authentication timed out"));
    }

    Ok(())
}

fn candidate_error(message: impl Into<String>) -> AuthenticateFailure {
    AuthenticateFailure::Candidate(transport_error(message))
}

fn validate_worker_ready(
    payload: &WorkerReadyPayload,
    worker_token: &str,
) -> Result<(), SupervisorError> {
    if payload.worker.is_empty() {
        return Err(transport_error("Go worker identity is empty"));
    }
    if payload.auth_token != worker_token {
        return Err(transport_error("Go worker authentication failed"));
    }
    if !payload
        .capabilities
        .iter()
        .any(|value| value == "lifecycle")
    {
        return Err(transport_error(
            "Go worker did not advertise lifecycle capability",
        ));
    }

    Ok(())
}

fn start_session(
    reader: BufReader<TcpStream>,
    decoder: Decoder,
    mut writer: TcpStream,
) -> (
    Receiver<Result<WorkerSignal, SupervisorError>>,
    WorkerSession,
) {
    let (sender, receiver) = mpsc::channel();
    let _ = sender.send(Ok(WorkerSignal::Ready));
    let reader_sender = sender.clone();
    let reader_thread = thread::spawn(move || {
        if let Err(error) = read_worker_events(reader, &reader_sender, decoder) {
            let _ = reader_sender.send(Err(error));
        }
    });
    let (commands, command_receiver) = mpsc::channel();
    let writer_thread = thread::spawn(move || {
        while let Ok(command) = command_receiver.recv() {
            let result = match command {
                WorkerCommand::Run(run) => write_envelope(
                    &mut writer,
                    Kind::Run,
                    Some(REQUEST_ID.to_owned()),
                    None,
                    &run,
                ),
                WorkerCommand::Shutdown => {
                    write_envelope(&mut writer, Kind::Shutdown, None, None, &ShutdownPayload {})
                }
            };
            if let Err(error) = result {
                let _ = sender.send(Err(error));
                return;
            }
        }
    });

    (
        receiver,
        WorkerSession {
            commands,
            reader_thread,
            writer_thread,
        },
    )
}

fn read_worker_events(
    mut reader: BufReader<TcpStream>,
    sender: &mpsc::Sender<Result<WorkerSignal, SupervisorError>>,
    mut decoder: Decoder,
) -> Result<(), SupervisorError> {
    let mut saw_lifecycle = false;
    let mut saw_complete = false;
    loop {
        let record = read_record(&mut reader)?;
        let envelope = decoder
            .decode_line(&record)
            .map_err(|error| transport_error(format!("decode Go worker event: {error}")))?;
        match envelope.kind {
            Kind::Lifecycle => {
                require_request_id(&envelope)?;
                let _: LifecyclePayload = decode_payload(&envelope).map_err(|error| {
                    transport_error(format!("decode Go worker lifecycle event: {error}"))
                })?;
                saw_lifecycle = true;
            }
            Kind::Complete if saw_lifecycle && !saw_complete => {
                require_request_id(&envelope)?;
                let payload: CompletePayload = decode_payload(&envelope).map_err(|error| {
                    transport_error(format!("decode Go worker completion: {error}"))
                })?;
                sender
                    .send(Ok(WorkerSignal::Complete(payload)))
                    .map_err(|_| transport_error("worker supervisor stopped receiving events"))?;
                saw_complete = true;
            }
            Kind::ShutdownAck if saw_complete => {
                let _: ShutdownPayload = decode_payload(&envelope).map_err(|error| {
                    transport_error(format!(
                        "decode Go worker shutdown acknowledgement: {error}"
                    ))
                })?;
                sender
                    .send(Ok(WorkerSignal::ShutdownAcknowledged))
                    .map_err(|_| transport_error("worker supervisor stopped receiving events"))?;
                return Ok(());
            }
            Kind::Error => {
                let payload: ErrorPayload = decode_payload(&envelope).map_err(|error| {
                    transport_error(format!("decode Go worker error event: {error}"))
                })?;
                return Err(transport_error(format!(
                    "Go worker error {}: {}",
                    payload.code, payload.message
                )));
            }
            kind => {
                return Err(transport_error(format!(
                    "unexpected Go worker event: {kind:?}"
                )));
            }
        }
    }
}

fn require_request_id(envelope: &Envelope) -> Result<(), SupervisorError> {
    if envelope.request_id.as_deref() != Some(REQUEST_ID) {
        return Err(transport_error("Go worker event has the wrong request_id"));
    }

    Ok(())
}

fn read_record(reader: &mut BufReader<TcpStream>) -> Result<Vec<u8>, SupervisorError> {
    let mut record = Vec::new();
    let mut limited = reader.take((MAX_LINE_BYTES + 2) as u64);
    let count = limited
        .read_until(b'\n', &mut record)
        .map_err(|error| transport_error(format!("read Go worker event: {error}")))?;
    if count == 0 {
        return Err(transport_error(
            "Go worker closed the protocol without completion",
        ));
    }
    if record.last() != Some(&b'\n') {
        let message = if record.len() >= MAX_LINE_BYTES + 2 {
            "Go worker event exceeds the protocol limit"
        } else {
            "Go worker event is not newline terminated"
        };
        return Err(transport_error(message));
    }

    Ok(record)
}

fn write_envelope<T: serde::Serialize>(
    writer: &mut TcpStream,
    kind: Kind,
    request_id: Option<String>,
    sequence: Option<u64>,
    payload: &T,
) -> Result<(), SupervisorError> {
    let envelope = Envelope::new(kind, request_id, sequence, payload)
        .map_err(|error| transport_error(format!("build worker message: {error}")))?;
    let record = encode(&envelope)
        .map_err(|error| transport_error(format!("encode worker message: {error}")))?;
    writer
        .write_all(&record)
        .map_err(|error| transport_error(format!("write worker message: {error}")))
}

#[derive(Clone, Copy, Eq, PartialEq)]
enum Control {
    Continue,
    Cancelled,
    RssLimit,
    Timeout,
}

fn control_state(config: &Config, started: Instant, cancellation: &Cancellation) -> Control {
    match cancellation.cause() {
        CAUSE_RSS => Control::RssLimit,
        CAUSE_CANCELLED => Control::Cancelled,
        _ if config
            .timeout
            .is_some_and(|timeout| started.elapsed() >= timeout) =>
        {
            Control::Timeout
        }
        _ => Control::Continue,
    }
}

fn finish_control(
    config: &Config,
    started: Instant,
    cancellation: Cancellation,
    memory: MemoryResult,
    control: Control,
) -> Result<i32, SupervisorError> {
    match control {
        Control::Timeout => finish_timeout(config, started, memory),
        Control::Cancelled | Control::RssLimit => {
            finish_cancelled(config, started, cancellation, memory)
        }
        Control::Continue => unreachable!(),
    }
}

fn finish_worker_status(
    config: &Config,
    started: Instant,
    memory: MemoryResult,
    status: ExitStatus,
    complete: Option<CompletePayload>,
) -> Result<i32, SupervisorError> {
    let Some(complete) = complete else {
        return finish_protocol_failure(
            config,
            started,
            memory,
            format!("Go worker exited without completion: {status}"),
        );
    };
    let exit_code = i32::try_from(complete.exit_code)
        .map_err(|_| transport_error("Go worker completion exit code is invalid"))?;
    if status.code() != Some(exit_code) {
        return finish_protocol_failure(
            config,
            started,
            memory,
            format!("Go worker status {status} disagrees with completion exit code {exit_code}"),
        );
    }
    let report = Report::completed(
        "exited",
        Some(exit_code),
        None,
        started.elapsed(),
        memory,
        None,
    );
    write_optional_report(config.report.as_deref(), &report)?;

    Ok(exit_code)
}

#[allow(clippy::too_many_arguments)]
fn fail_session(
    config: &Config,
    started: Instant,
    mut child: ManagedChild,
    monitor: Option<MemoryMonitor>,
    session: WorkerSession,
    message: String,
) -> Result<i32, SupervisorError> {
    let terminate = child.terminate().err();
    let memory = stop_monitor(monitor).unwrap_or_else(|_| MemoryResult::unknown());
    if terminate.is_none() {
        session.finish();
    }

    finish_protocol_failure(
        config,
        started,
        memory,
        append_cleanup_error(message, terminate),
    )
}

fn finish_protocol_failure(
    config: &Config,
    started: Instant,
    memory: MemoryResult,
    message: String,
) -> Result<i32, SupervisorError> {
    let report = Report::lifecycle_error(started.elapsed(), memory, message.clone());
    write_optional_report(config.report.as_deref(), &report)?;
    Err(SupervisorError::new(EXIT_SUPERVISOR, message))
}

fn append_cleanup_error(message: String, cleanup: Option<SupervisorError>) -> String {
    cleanup.map_or(message.clone(), |error| {
        format!("{message}; process-tree cleanup failed: {error}")
    })
}

fn generate_worker_token() -> Result<String, SupervisorError> {
    let mut bytes = [0_u8; 32];
    fill_random(&mut bytes)?;
    const HEX: &[u8; 16] = b"0123456789abcdef";
    let mut token = String::with_capacity(bytes.len() * 2);
    for byte in bytes {
        token.push(char::from(HEX[usize::from(byte >> 4)]));
        token.push(char::from(HEX[usize::from(byte & 0x0f)]));
    }

    Ok(token)
}

#[cfg(unix)]
fn fill_random(bytes: &mut [u8]) -> Result<(), SupervisorError> {
    let mut source = File::open("/dev/urandom")
        .map_err(|error| transport_error(format!("open system random source: {error}")))?;
    source
        .read_exact(bytes)
        .map_err(|error| transport_error(format!("read system random source: {error}")))
}

#[cfg(windows)]
fn fill_random(bytes: &mut [u8]) -> Result<(), SupervisorError> {
    let length = u32::try_from(bytes.len()).expect("worker token length fits u32");
    // The null algorithm handle selects the system RNG with this flag.
    let status = unsafe {
        BCryptGenRandom(
            std::ptr::null_mut(),
            bytes.as_mut_ptr(),
            length,
            BCRYPT_USE_SYSTEM_PREFERRED_RNG,
        )
    };
    if status != 0 {
        return Err(transport_error(format!(
            "generate worker authentication token: NTSTATUS {status:#x}"
        )));
    }

    Ok(())
}

fn transport_error(message: impl Into<String>) -> SupervisorError {
    SupervisorError::new(EXIT_SUPERVISOR, message)
}
