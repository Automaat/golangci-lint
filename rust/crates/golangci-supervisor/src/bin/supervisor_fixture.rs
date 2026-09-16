use std::{
    collections::BTreeMap,
    env,
    io::{BufRead, BufReader, Write},
    net::TcpStream,
    process::Command,
    thread,
    time::Duration,
};

use golangci_protocol::{
    CompletePayload, Decoder, Envelope, HelloPayload, Kind, LifecyclePayload, RunPayload,
    ShutdownPayload, decode_payload, encode,
};

const WORKER_ENDPOINT_ENV: &str = "GOLANGCI_WORKER_ENDPOINT";
const WORKER_TOKEN_ENV: &str = "GOLANGCI_WORKER_TOKEN";
const BAD_AUTH_ENV: &str = "GOLANGCI_FIXTURE_BAD_AUTH";
const DECOY_ENV: &str = "GOLANGCI_FIXTURE_DECOY";
const EARLY_EXIT_ENV: &str = "GOLANGCI_FIXTURE_EARLY_EXIT";
const STOP_AFTER_READY_ENV: &str = "GOLANGCI_FIXTURE_STOP_AFTER_READY";

#[derive(serde::Serialize)]
struct WorkerReadyPayload {
    worker: String,
    capabilities: Vec<String>,
    auth_token: String,
}

fn main() {
    if env::var_os(EARLY_EXIT_ENV).is_some() {
        std::process::exit(9);
    }
    if let Some(endpoint) = env::var_os(WORKER_ENDPOINT_ENV) {
        let exit_code = protocol_worker(endpoint.to_string_lossy().as_ref());
        std::process::exit(exit_code);
    }

    let mut args = env::args().skip(1);
    match args.next().as_deref() {
        Some("echo") => {
            println!("stdout:{}", args.next().unwrap_or_default());
            eprintln!("stderr:fixture");
            std::process::exit(args.next().and_then(|v| v.parse().ok()).unwrap_or(0));
        }
        Some("sleep") => sleep(args.next()),
        Some("allocate") => allocate(args.next(), args.next()),
        Some("spawn-descendant") => spawn_descendant(args.next(), args.next()),
        Some("run") => run_fixture(args.next().as_deref()),
        _ => std::process::exit(2),
    }
}

fn protocol_worker(endpoint: &str) -> i32 {
    let expected_token = env::var(WORKER_TOKEN_ENV).expect("worker authentication token");
    if env::var_os(DECOY_ENV).is_some() {
        connect_decoy(endpoint);
    }
    let mut stream = TcpStream::connect(endpoint).expect("connect controller");
    let mut reader = BufReader::new(stream.try_clone().expect("clone controller stream"));
    let mut decoder = Decoder::default();

    let hello = read_envelope(&mut reader, &mut decoder);
    assert_eq!(hello.kind, Kind::Hello);
    let hello_payload: HelloPayload = decode_payload(&hello).expect("decode hello");
    assert_eq!(hello_payload.client, "golangci-supervisor");
    assert!(
        hello_payload
            .capabilities
            .iter()
            .any(|value| value == "lifecycle")
    );
    let auth_token = if env::var_os(BAD_AUTH_ENV).is_some() {
        "invalid-token".to_owned()
    } else {
        expected_token
    };
    write_envelope(
        &mut stream,
        Kind::Ready,
        None,
        Some(1),
        &WorkerReadyPayload {
            worker: "supervisor-fixture".to_owned(),
            capabilities: vec!["lifecycle".to_owned()],
            auth_token,
        },
    );
    if env::var_os(STOP_AFTER_READY_ENV).is_some() {
        thread::sleep(Duration::from_secs(5));
        return 0;
    }

    let run = read_envelope(&mut reader, &mut decoder);
    assert_eq!(run.kind, Kind::Run);
    let request_id = run.request_id.clone().expect("run request ID");
    let payload: RunPayload = decode_payload(&run).expect("decode run payload");
    let behavior = payload.args.get(1).map(String::as_str).unwrap_or_default();
    let stdin_exit_code = (behavior == "protocol-stdin").then(stdin_fixture);
    match behavior {
        "protocol-malformed" => {
            stream.write_all(b"{broken\n").expect("write malformed");
            return 0;
        }
        "protocol-oversized" => {
            stream
                .write_all(&vec![b'x'; golangci_protocol::MAX_LINE_BYTES + 2])
                .expect("write oversized");
            return 0;
        }
        "protocol-sleep" => {
            thread::sleep(Duration::from_secs(5));
            return 0;
        }
        "protocol-allocate" => {
            allocate(Some("64".to_owned()), Some("5000".to_owned()));
            return 0;
        }
        "protocol-spawn-descendant" => {
            spawn_descendant(payload.args.get(2).cloned(), Some("5000".to_owned()));
            return 0;
        }
        _ => {}
    }

    let event_request_id = if behavior == "protocol-wrong-request" {
        "wrong-run".to_owned()
    } else {
        request_id.clone()
    };
    if behavior != "protocol-missing-lifecycle" {
        write_envelope(
            &mut stream,
            Kind::Lifecycle,
            Some(event_request_id),
            Some(2),
            &LifecyclePayload {
                phase: "analysis".to_owned(),
                state: "completed".to_owned(),
                elapsed_ns: 1,
                metrics: BTreeMap::from([("issues".to_owned(), 0)]),
                error: String::new(),
            },
        );
    }
    if behavior == "protocol-missing-complete" || behavior == "protocol-wrong-request" {
        return 0;
    }

    let exit_code = stdin_exit_code.unwrap_or_else(|| fixture_result(behavior));
    write_envelope(
        &mut stream,
        Kind::Complete,
        Some(request_id),
        Some(if behavior == "protocol-missing-lifecycle" {
            2
        } else {
            3
        }),
        &CompletePayload {
            exit_code: i64::from(exit_code),
            issues: if exit_code == 1 { 1 } else { 0 },
            elapsed_ns: 1,
            context_error: String::new(),
        },
    );
    if behavior == "protocol-ignore-shutdown" {
        thread::sleep(Duration::from_secs(5));
        return exit_code;
    }
    if behavior == "protocol-close-before-shutdown" {
        drop(reader);
        drop(stream);
        thread::sleep(Duration::from_millis(250));
        return exit_code;
    }
    let shutdown = read_envelope(&mut reader, &mut decoder);
    assert_eq!(shutdown.kind, Kind::Shutdown);
    let _: ShutdownPayload = decode_payload(&shutdown).expect("decode shutdown payload");
    write_envelope(
        &mut stream,
        Kind::ShutdownAck,
        None,
        Some(4),
        &ShutdownPayload {},
    );

    exit_code
}

fn connect_decoy(endpoint: &str) {
    let endpoint = endpoint.to_owned();
    let (connected, ready) = std::sync::mpsc::channel();
    let _decoy = thread::spawn(move || {
        let _stream = TcpStream::connect(endpoint).expect("connect decoy");
        connected.send(()).expect("signal decoy connection");
        thread::sleep(Duration::from_secs(2));
    });
    ready.recv().expect("wait for decoy connection");
}

fn read_envelope(reader: &mut impl BufRead, decoder: &mut Decoder) -> Envelope {
    let mut line = Vec::new();
    reader.read_until(b'\n', &mut line).expect("read envelope");
    decoder.decode_line(&line).expect("decode envelope")
}

fn write_envelope<T: serde::Serialize>(
    stream: &mut TcpStream,
    kind: Kind,
    request_id: Option<String>,
    sequence: Option<u64>,
    payload: &T,
) {
    let envelope = Envelope::new(kind, request_id, sequence, payload).expect("build envelope");
    stream
        .write_all(&encode(&envelope).expect("encode envelope"))
        .expect("write envelope");
}

fn run_fixture(behavior: Option<&str>) {
    if behavior == Some("protocol-stdin") {
        std::process::exit(stdin_fixture());
    }
    let exit_code = fixture_result(behavior.unwrap_or_default());
    std::process::exit(exit_code);
}

fn stdin_fixture() -> i32 {
    let mut input = String::new();
    std::io::stdin().read_line(&mut input).expect("read stdin");
    print!("stdin:{input}");
    7
}

fn fixture_result(behavior: &str) -> i32 {
    match behavior {
        "protocol-success" => {
            println!("stdout:protocol");
            eprintln!("stderr:protocol");
            7
        }
        "protocol-issues" => {
            println!("issue:protocol");
            1
        }
        _ => 0,
    }
}

fn sleep(millis: Option<String>) {
    thread::sleep(Duration::from_millis(parse_u64(millis, 5_000)));
}

fn allocate(mebibytes: Option<String>, millis: Option<String>) {
    let mut bytes = vec![0_u8; parse_u64(mebibytes, 64) as usize * 1024 * 1024];
    for byte in bytes.iter_mut().step_by(4096) {
        *byte = 1;
    }
    std::hint::black_box(&bytes);
    sleep(millis);
}

fn spawn_descendant(pid_file: Option<String>, millis: Option<String>) {
    let millis = millis.unwrap_or_else(|| "5000".to_owned());
    let mut child = Command::new(env::current_exe().expect("fixture executable"))
        .arg("sleep")
        .arg(&millis)
        .spawn()
        .expect("spawn descendant");
    std::fs::write(pid_file.expect("pid file"), child.id().to_string()).expect("write pid file");
    sleep(Some(millis));
    child.wait().expect("reap descendant");
}

fn parse_u64(value: Option<String>, default: u64) -> u64 {
    value.and_then(|v| v.parse().ok()).unwrap_or(default)
}
