use std::fs;
use std::path::{Path, PathBuf};

use golangci_protocol::{
    Decoder, Envelope, ErrorClass, ErrorPayload, Kind, MAX_LINE_BYTES, encode,
};
use serde::Deserialize;

#[derive(Deserialize)]
struct InvalidVector {
    name: String,
    error_class: ErrorClass,
    #[serde(default)]
    records: Vec<String>,
    #[serde(default)]
    oversized_bytes: usize,
    #[serde(default)]
    hex_record: String,
    #[serde(default)]
    nested_arrays: usize,
}

#[derive(Deserialize)]
struct NormalizationVector {
    name: String,
    input: String,
    canonical: String,
}

#[test]
fn golden_streams_round_trip() {
    let mut paths = fs::read_dir(testdata_dir())
        .expect("read testdata")
        .map(|entry| entry.expect("read directory entry").path())
        .filter(|path| {
            path.extension()
                .is_some_and(|extension| extension == "jsonl")
        })
        .collect::<Vec<_>>();
    paths.sort();
    assert!(!paths.is_empty());

    for path in paths {
        let input = fs::read_to_string(&path).expect("read golden stream");
        let mut decoder = Decoder::default();
        for line in input.lines() {
            let record = format!("{line}\n");
            let envelope = decoder
                .decode_line(record.as_bytes())
                .unwrap_or_else(|error| panic!("{}: {error}", path.display()));
            assert_eq!(
                encode(&envelope).expect("encode envelope"),
                record.as_bytes()
            );
        }
    }
}

#[test]
fn invalid_golden_vectors_have_stable_classes() {
    let input =
        fs::read_to_string(testdata_dir().join("invalid.json")).expect("read invalid vectors");
    let vectors: Vec<InvalidVector> = serde_json::from_str(&input).expect("decode invalid vectors");
    assert!(!vectors.is_empty());

    for vector in vectors {
        let mut decoder = Decoder::default();
        let mut result = None;
        let last_record = vector.records.len().saturating_sub(1);
        for (index, record) in vector.records.into_iter().enumerate() {
            result = Some(decoder.decode_line(format!("{record}\n").as_bytes()));
            if index < last_record {
                assert!(
                    result.as_ref().is_some_and(Result::is_ok),
                    "{} setup record {index} failed",
                    vector.name
                );
            }
        }
        if vector.oversized_bytes > 0 {
            result = Some(decoder.decode_line(&vec![b'x'; vector.oversized_bytes]));
        }
        if !vector.hex_record.is_empty() {
            let record = decode_hex(&vector.hex_record);
            result = Some(decoder.decode_line(&record));
        }
        if vector.nested_arrays > 0 {
            let prefix = r#"{"protocol_version":1,"kind":"hello","payload":{"client":"controller","capabilities":["diagnostics"],"future":"#;
            let build = |depth: usize| {
                let mut record = String::from(prefix);
                record.push_str(&"[".repeat(depth));
                record.push('0');
                record.push_str(&"]".repeat(depth));
                record.push_str("}}");
                record
            };
            Decoder::default()
                .decode_line(build(vector.nested_arrays - 1).as_bytes())
                .expect("accept maximum nesting depth");
            let record = build(vector.nested_arrays);
            result = Some(decoder.decode_line(record.as_bytes()));
        }

        let error = result
            .unwrap_or_else(|| panic!("{} has no input", vector.name))
            .expect_err(&vector.name);
        assert_eq!(error.class(), vector.error_class, "{}", vector.name);
    }
}

#[test]
fn normalization_vectors_are_canonical() {
    let input = fs::read_to_string(testdata_dir().join("normalization.json"))
        .expect("read normalization vectors");
    let vectors: Vec<NormalizationVector> =
        serde_json::from_str(&input).expect("decode normalization vectors");
    assert!(!vectors.is_empty());

    for vector in vectors {
        assert_ne!(vector.input, vector.canonical, "{}", vector.name);
        let envelope = Decoder::default()
            .decode_line(vector.input.as_bytes())
            .unwrap_or_else(|error| panic!("{}: {error}", vector.name));
        assert_eq!(
            encode(&envelope).expect("encode normalized envelope"),
            vector.canonical.as_bytes(),
            "{}",
            vector.name
        );
    }
}

#[test]
fn typed_envelope_uses_canonical_json() {
    let envelope = Envelope::new(
        Kind::Error,
        None,
        Some(1),
        &ErrorPayload {
            code: "bad_input".to_owned(),
            message: "line\u{2028}paragraph\u{2029}end".to_owned(),
            fatal: false,
        },
    )
    .expect("construct envelope");

    assert_eq!(
        encode(&envelope).expect("encode envelope"),
        b"{\"protocol_version\":1,\"kind\":\"error\",\"sequence\":1,\"payload\":{\"code\":\"bad_input\",\"message\":\"line\\u2028paragraph\\u2029end\",\"fatal\":false}}\n"
    );
}

#[test]
fn maximum_size_matches_contract() {
    assert_eq!(MAX_LINE_BYTES, 1_048_576);

    let prefix = r#"{"protocol_version":1,"kind":"shutdown","payload":{"padding":""#;
    let suffix = r#""}}"#;
    let record = format!(
        "{prefix}{}{suffix}",
        "x".repeat(MAX_LINE_BYTES - prefix.len() - suffix.len())
    );
    assert_eq!(record.len(), MAX_LINE_BYTES);

    let envelope = Decoder::default()
        .decode_line(format!("{record}\n").as_bytes())
        .expect("decode maximum record");
    assert_eq!(
        encode(&envelope).expect("encode maximum record"),
        format!("{record}\n").as_bytes()
    );
}

#[test]
fn escaped_separator_obeys_maximum_size() {
    let prefix = r#"{"protocol_version":1,"kind":"error","sequence":1,"payload":{"code":"boundary","message":""#;
    let suffix = r#"","fatal":true}}"#;
    let build = |padding: usize| {
        let record = format!("{prefix}{}\u{2028}{suffix}\n", "x".repeat(padding));
        Decoder::default()
            .decode_line(record.as_bytes())
            .expect("decode boundary envelope")
    };

    let base = encode(&build(0)).expect("encode base envelope");
    let padding = MAX_LINE_BYTES - (base.len() - 1);

    let encoded = encode(&build(padding)).expect("encode maximum envelope");
    assert_eq!(encoded.len(), MAX_LINE_BYTES + 1);

    let error = encode(&build(padding + 1)).expect_err("reject oversized envelope");
    assert_eq!(error.class(), ErrorClass::TooLarge);
}

fn testdata_dir() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR")).join("../../../testdata/worker-protocol/v1")
}

fn decode_hex(value: &str) -> Vec<u8> {
    value
        .as_bytes()
        .chunks_exact(2)
        .map(|chunk| {
            let digits = std::str::from_utf8(chunk).expect("hex digits are UTF-8");
            u8::from_str_radix(digits, 16).expect("valid hex record")
        })
        .collect()
}
