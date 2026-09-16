use std::{env, process::Command, thread, time::Duration};

fn main() {
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
        _ => std::process::exit(2),
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
