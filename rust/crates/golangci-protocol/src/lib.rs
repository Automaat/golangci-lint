//! Versioned JSON-lines contract between the Rust controller and Go worker.

use std::collections::{BTreeMap, BTreeSet};
use std::error::Error;
use std::fmt;

use serde::de::DeserializeOwned;
use serde::{Deserialize, Serialize};
use serde_json::value::{RawValue, to_raw_value};

pub const PROTOCOL_VERSION: i64 = 1;
pub const MAX_LINE_BYTES: usize = 1 << 20;
pub const MAX_NESTING_DEPTH: usize = 64;

#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum Kind {
    Hello,
    Ready,
    Run,
    Cancel,
    Shutdown,
    ShutdownAck,
    Lifecycle,
    Diagnostic,
    Complete,
    Error,
    #[serde(other)]
    Unknown,
}

impl Kind {
    fn is_worker_event(self) -> bool {
        matches!(
            self,
            Self::Ready
                | Self::ShutdownAck
                | Self::Lifecycle
                | Self::Diagnostic
                | Self::Complete
                | Self::Error
        )
    }

    fn is_run_scoped(self) -> bool {
        matches!(
            self,
            Self::Run | Self::Cancel | Self::Lifecycle | Self::Diagnostic | Self::Complete
        )
    }
}

#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum ErrorClass {
    TooLarge,
    MalformedJson,
    UnsupportedVersion,
    UnknownKind,
    InvalidEnvelope,
    InvalidPayload,
    InvalidSequence,
}

#[derive(Debug)]
pub struct ProtocolError {
    class: ErrorClass,
    detail: String,
}

impl ProtocolError {
    #[must_use]
    pub const fn class(&self) -> ErrorClass {
        self.class
    }

    fn new(class: ErrorClass, detail: impl Into<String>) -> Self {
        Self {
            class,
            detail: detail.into(),
        }
    }
}

impl fmt::Display for ProtocolError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(
            formatter,
            "worker protocol {:?}: {}",
            self.class, self.detail
        )
    }
}

impl Error for ProtocolError {}

#[derive(Debug, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct Envelope {
    pub protocol_version: i64,
    pub kind: Kind,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub request_id: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub sequence: Option<u64>,
    pub payload: Box<RawValue>,
}

impl Envelope {
    pub fn new<T: Serialize>(
        kind: Kind,
        request_id: Option<String>,
        sequence: Option<u64>,
        payload: &T,
    ) -> Result<Self, ProtocolError> {
        let envelope = Self {
            protocol_version: PROTOCOL_VERSION,
            kind,
            request_id,
            sequence,
            payload: to_raw_value(payload).map_err(|error| {
                ProtocolError::new(ErrorClass::InvalidPayload, error.to_string())
            })?,
        };
        validate_envelope(&envelope)?;

        Ok(envelope)
    }
}

#[derive(Debug, Deserialize, Serialize)]
pub struct HelloPayload {
    pub client: String,
    pub capabilities: Vec<String>,
}

#[derive(Debug, Deserialize, Serialize)]
pub struct ReadyPayload {
    pub worker: String,
    pub capabilities: Vec<String>,
}

#[derive(Debug, Deserialize, Serialize)]
pub struct RunPayload {
    pub args: Vec<String>,
    pub working_directory: String,
    pub environment: BTreeMap<String, String>,
}

#[derive(Debug, Deserialize, Serialize)]
pub struct CancelPayload {
    pub reason: String,
}

#[derive(Debug, Deserialize, Serialize)]
pub struct ShutdownPayload {}

#[derive(Debug, Deserialize, Serialize)]
pub struct LifecyclePayload {
    pub phase: String,
    pub state: String,
    pub elapsed_ns: i64,
    pub metrics: BTreeMap<String, i64>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub error: String,
}

#[derive(Debug, Deserialize, Serialize)]
pub struct DiagnosticPayload {
    pub linter: String,
    pub message: String,
    pub path: String,
    pub line: i64,
    pub column: i64,
    pub severity: String,
}

#[derive(Debug, Deserialize, Serialize)]
pub struct CompletePayload {
    pub exit_code: i64,
    pub issues: i64,
    pub elapsed_ns: i64,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub context_error: String,
}

#[derive(Debug, Deserialize, Serialize)]
pub struct ErrorPayload {
    pub code: String,
    pub message: String,
    pub fatal: bool,
}

#[derive(Default)]
pub struct Decoder {
    last_sequence: u64,
}

impl Decoder {
    pub fn decode_line(&mut self, line: &[u8]) -> Result<Envelope, ProtocolError> {
        let record = trim_terminator(line);
        if record.len() > MAX_LINE_BYTES {
            return Err(ProtocolError::new(
                ErrorClass::TooLarge,
                format!("record is {} bytes", record.len()),
            ));
        }
        validate_json_structure(record)?;
        validate_required_envelope_fields(record)?;

        let envelope: Envelope = serde_json::from_slice(record).map_err(classify_json_error)?;
        validate_envelope(&envelope)?;

        if envelope.kind.is_worker_event() {
            let sequence = envelope.sequence.expect("validated worker sequence");
            if sequence <= self.last_sequence {
                return Err(ProtocolError::new(
                    ErrorClass::InvalidSequence,
                    format!("sequence {sequence} does not follow {}", self.last_sequence),
                ));
            }
            self.last_sequence = sequence;
        }

        Ok(envelope)
    }
}

pub fn encode(envelope: &Envelope) -> Result<Vec<u8>, ProtocolError> {
    validate_envelope(envelope)?;

    let record = serde_json::to_vec(envelope)
        .map_err(|error| ProtocolError::new(ErrorClass::InvalidEnvelope, error.to_string()))?;
    validate_json_structure(&record)?;
    let mut record = escape_line_separators(compact_json(&record));
    if record.len() > MAX_LINE_BYTES {
        return Err(ProtocolError::new(
            ErrorClass::TooLarge,
            format!("encoded record exceeds {MAX_LINE_BYTES} bytes"),
        ));
    }
    record.push(b'\n');

    Ok(record)
}

pub fn decode_payload<T: DeserializeOwned>(envelope: &Envelope) -> Result<T, ProtocolError> {
    serde_json::from_str(envelope.payload.get()).map_err(|error| {
        ProtocolError::new(
            ErrorClass::InvalidPayload,
            format!("decode {:?} payload: {error}", envelope.kind),
        )
    })
}

fn trim_terminator(mut line: &[u8]) -> &[u8] {
    if let Some(stripped) = line.strip_suffix(b"\n") {
        line = stripped;
    }
    if let Some(stripped) = line.strip_suffix(b"\r") {
        line = stripped;
    }

    line
}

#[derive(Clone, Copy)]
enum JsonStructureIssue {
    Duplicate,
    TooDeep,
    Malformed,
}

fn validate_json_structure(record: &[u8]) -> Result<(), ProtocolError> {
    if std::str::from_utf8(record).is_err() {
        return Ok(());
    }

    let mut offset = 0;
    match scan_json_value(record, &mut offset, 1) {
        Ok(()) => {
            skip_json_whitespace(record, &mut offset);
            if offset == record.len() && has_unpaired_surrogate_escape(record) {
                return Err(ProtocolError::new(
                    ErrorClass::InvalidEnvelope,
                    "record contains an unpaired UTF-16 surrogate",
                ));
            }

            Ok(())
        }
        Err(JsonStructureIssue::Malformed) => Ok(()),
        Err(JsonStructureIssue::Duplicate) => Err(ProtocolError::new(
            ErrorClass::InvalidEnvelope,
            "record contains a duplicate object key",
        )),
        Err(JsonStructureIssue::TooDeep) => Err(ProtocolError::new(
            ErrorClass::InvalidEnvelope,
            format!("record exceeds nesting depth {MAX_NESTING_DEPTH}"),
        )),
    }
}

fn scan_json_value(
    record: &[u8],
    offset: &mut usize,
    depth: usize,
) -> Result<(), JsonStructureIssue> {
    skip_json_whitespace(record, offset);
    match record.get(*offset).copied() {
        Some(b'{') => scan_json_object(record, offset, depth),
        Some(b'[') => scan_json_array(record, offset, depth),
        Some(b'"') => scan_json_string(record, offset).map(|_| ()),
        Some(_) => scan_json_primitive(record, offset),
        None => Err(JsonStructureIssue::Malformed),
    }
}

fn scan_json_object(
    record: &[u8],
    offset: &mut usize,
    depth: usize,
) -> Result<(), JsonStructureIssue> {
    if depth > MAX_NESTING_DEPTH {
        return Err(JsonStructureIssue::TooDeep);
    }
    *offset += 1;
    skip_json_whitespace(record, offset);
    if record.get(*offset) == Some(&b'}') {
        *offset += 1;
        return Ok(());
    }

    let mut keys = BTreeSet::new();
    loop {
        let raw_key = scan_json_string(record, offset)?;
        if !has_unpaired_surrogate_escape(raw_key) {
            let key: String =
                serde_json::from_slice(raw_key).map_err(|_| JsonStructureIssue::Malformed)?;
            if !keys.insert(key) {
                return Err(JsonStructureIssue::Duplicate);
            }
        }
        skip_json_whitespace(record, offset);
        if record.get(*offset) != Some(&b':') {
            return Err(JsonStructureIssue::Malformed);
        }
        *offset += 1;
        scan_json_value(record, offset, depth + 1)?;
        skip_json_whitespace(record, offset);
        match record.get(*offset).copied() {
            Some(b',') => *offset += 1,
            Some(b'}') => {
                *offset += 1;
                return Ok(());
            }
            _ => return Err(JsonStructureIssue::Malformed),
        }
        skip_json_whitespace(record, offset);
    }
}

fn scan_json_array(
    record: &[u8],
    offset: &mut usize,
    depth: usize,
) -> Result<(), JsonStructureIssue> {
    if depth > MAX_NESTING_DEPTH {
        return Err(JsonStructureIssue::TooDeep);
    }
    *offset += 1;
    skip_json_whitespace(record, offset);
    if record.get(*offset) == Some(&b']') {
        *offset += 1;
        return Ok(());
    }

    loop {
        scan_json_value(record, offset, depth + 1)?;
        skip_json_whitespace(record, offset);
        match record.get(*offset).copied() {
            Some(b',') => *offset += 1,
            Some(b']') => {
                *offset += 1;
                return Ok(());
            }
            _ => return Err(JsonStructureIssue::Malformed),
        }
        skip_json_whitespace(record, offset);
    }
}

fn scan_json_string<'a>(
    record: &'a [u8],
    offset: &mut usize,
) -> Result<&'a [u8], JsonStructureIssue> {
    let start = *offset;
    if record.get(*offset) != Some(&b'"') {
        return Err(JsonStructureIssue::Malformed);
    }
    *offset += 1;
    while let Some(&value) = record.get(*offset) {
        match value {
            b'"' => {
                *offset += 1;
                return Ok(&record[start..*offset]);
            }
            b'\\' => scan_json_escape(record, offset)?,
            0x00..=0x1f => return Err(JsonStructureIssue::Malformed),
            _ => *offset += 1,
        }
    }

    Err(JsonStructureIssue::Malformed)
}

fn scan_json_escape(record: &[u8], offset: &mut usize) -> Result<(), JsonStructureIssue> {
    match record.get(*offset + 1).copied() {
        Some(b'"' | b'\\' | b'/' | b'b' | b'f' | b'n' | b'r' | b't') => *offset += 2,
        Some(b'u') => {
            let digits = record
                .get(*offset + 2..*offset + 6)
                .ok_or(JsonStructureIssue::Malformed)?;
            if !digits.iter().all(u8::is_ascii_hexdigit) {
                return Err(JsonStructureIssue::Malformed);
            }
            *offset += 6;
        }
        _ => return Err(JsonStructureIssue::Malformed),
    }

    Ok(())
}

fn scan_json_primitive(record: &[u8], offset: &mut usize) -> Result<(), JsonStructureIssue> {
    let start = *offset;
    while let Some(value) = record.get(*offset) {
        if matches!(*value, b' ' | b'\t' | b'\r' | b'\n' | b',' | b']' | b'}') {
            break;
        }
        *offset += 1;
    }
    if *offset == start {
        return Err(JsonStructureIssue::Malformed);
    }
    serde_json::from_slice::<Box<RawValue>>(&record[start..*offset])
        .map_err(|_| JsonStructureIssue::Malformed)?;

    Ok(())
}

fn skip_json_whitespace(record: &[u8], offset: &mut usize) {
    while record
        .get(*offset)
        .is_some_and(|value| matches!(*value, b' ' | b'\t' | b'\r' | b'\n'))
    {
        *offset += 1;
    }
}

fn has_unpaired_surrogate_escape(data: &[u8]) -> bool {
    let mut in_string = false;
    let mut offset = 0;
    while offset < data.len() {
        let value = data[offset];
        if !in_string {
            in_string = value == b'"';
            offset += 1;
            continue;
        }
        if value == b'"' {
            in_string = false;
            offset += 1;
            continue;
        }
        if value != b'\\' || offset + 1 >= data.len() {
            offset += 1;
            continue;
        }
        if data[offset + 1] != b'u' {
            offset += 2;
            continue;
        }

        let Some(code) = decode_hex_quad(data, offset + 2) else {
            offset += 2;
            continue;
        };
        if (0xdc00..=0xdfff).contains(&code) {
            return true;
        }
        if !(0xd800..=0xdbff).contains(&code) {
            offset += 6;
            continue;
        }
        if offset + 12 > data.len() || data[offset + 6] != b'\\' || data[offset + 7] != b'u' {
            return true;
        }
        let Some(low) = decode_hex_quad(data, offset + 8) else {
            return true;
        };
        if !(0xdc00..=0xdfff).contains(&low) {
            return true;
        }
        offset += 12;
    }

    false
}

fn decode_hex_quad(data: &[u8], offset: usize) -> Option<u16> {
    let digits = data.get(offset..offset + 4)?;
    let mut value = 0_u16;
    for digit in digits {
        value <<= 4;
        value += match *digit {
            b'0'..=b'9' => u16::from(*digit - b'0'),
            b'a'..=b'f' => u16::from(*digit - b'a') + 10,
            b'A'..=b'F' => u16::from(*digit - b'A') + 10,
            _ => return None,
        };
    }

    Some(value)
}

fn escape_line_separators(record: Vec<u8>) -> Vec<u8> {
    if !record
        .windows(3)
        .any(|bytes| matches!(bytes, [0xe2, 0x80, 0xa8 | 0xa9]))
    {
        return record;
    }

    let mut escaped = Vec::with_capacity(record.len());
    let mut offset = 0;
    while offset < record.len() {
        match record.get(offset..offset + 3) {
            Some([0xe2, 0x80, 0xa8]) => {
                escaped.extend_from_slice(b"\\u2028");
                offset += 3;
            }
            Some([0xe2, 0x80, 0xa9]) => {
                escaped.extend_from_slice(b"\\u2029");
                offset += 3;
            }
            _ => {
                escaped.push(record[offset]);
                offset += 1;
            }
        }
    }

    escaped
}

fn compact_json(record: &[u8]) -> Vec<u8> {
    let mut compacted = Vec::with_capacity(record.len());
    let mut in_string = false;
    let mut escaped = false;
    for &value in record {
        if in_string {
            compacted.push(value);
            if escaped {
                escaped = false;
            } else if value == b'\\' {
                escaped = true;
            } else if value == b'"' {
                in_string = false;
            }
            continue;
        }
        if value == b'"' {
            in_string = true;
            compacted.push(value);
            continue;
        }
        if !matches!(value, b' ' | b'\t' | b'\r' | b'\n') {
            compacted.push(value);
        }
    }

    compacted
}

fn classify_json_error(error: serde_json::Error) -> ProtocolError {
    let class = match error.classify() {
        serde_json::error::Category::Data => ErrorClass::InvalidEnvelope,
        serde_json::error::Category::Io
        | serde_json::error::Category::Syntax
        | serde_json::error::Category::Eof => ErrorClass::MalformedJson,
    };

    ProtocolError::new(class, error.to_string())
}

fn validate_required_envelope_fields(record: &[u8]) -> Result<(), ProtocolError> {
    let object: BTreeMap<String, Box<RawValue>> =
        serde_json::from_slice(record).map_err(classify_json_error)?;
    for field in ["protocol_version", "kind", "payload"] {
        if !object.contains_key(field) {
            return Err(ProtocolError::new(
                ErrorClass::InvalidEnvelope,
                format!("envelope requires {field}"),
            ));
        }
    }
    for field in ["protocol_version", "kind"] {
        if object
            .get(field)
            .is_some_and(|value| value.get().trim() == "null")
        {
            return Err(ProtocolError::new(
                ErrorClass::InvalidEnvelope,
                format!("envelope requires non-null {field}"),
            ));
        }
    }

    Ok(())
}

fn validate_envelope(envelope: &Envelope) -> Result<(), ProtocolError> {
    if envelope.protocol_version != PROTOCOL_VERSION {
        return Err(ProtocolError::new(
            ErrorClass::UnsupportedVersion,
            format!("got {}, want {PROTOCOL_VERSION}", envelope.protocol_version),
        ));
    }
    if envelope.kind == Kind::Unknown {
        return Err(ProtocolError::new(
            ErrorClass::UnknownKind,
            "unknown message kind",
        ));
    }
    if envelope
        .request_id
        .as_ref()
        .is_some_and(std::string::String::is_empty)
    {
        return Err(ProtocolError::new(
            ErrorClass::InvalidEnvelope,
            "message forbids an empty request_id",
        ));
    }
    if envelope.kind.is_run_scoped()
        && envelope
            .request_id
            .as_ref()
            .is_none_or(std::string::String::is_empty)
    {
        return Err(ProtocolError::new(
            ErrorClass::InvalidEnvelope,
            "run-scoped message requires request_id",
        ));
    }
    if !envelope.kind.is_run_scoped()
        && envelope.kind != Kind::Error
        && envelope.request_id.is_some()
    {
        return Err(ProtocolError::new(
            ErrorClass::InvalidEnvelope,
            "message forbids request_id",
        ));
    }
    if envelope.kind.is_worker_event() && envelope.sequence.is_none_or(|sequence| sequence == 0) {
        return Err(ProtocolError::new(
            ErrorClass::InvalidEnvelope,
            "worker event requires a positive sequence",
        ));
    }
    if !envelope.kind.is_worker_event() && envelope.sequence.is_some() {
        return Err(ProtocolError::new(
            ErrorClass::InvalidEnvelope,
            "controller message forbids sequence",
        ));
    }

    validate_payload(envelope)
}

fn validate_payload(envelope: &Envelope) -> Result<(), ProtocolError> {
    if !envelope.payload.get().trim_start().starts_with('{') {
        return Err(ProtocolError::new(
            ErrorClass::InvalidPayload,
            "payload must be an object",
        ));
    }

    match envelope.kind {
        Kind::Hello => {
            let payload: HelloPayload = decode_payload(envelope)?;
            ensure(
                !payload.client.is_empty() && !payload.capabilities.is_empty(),
                "hello requires client and capabilities",
            )
        }
        Kind::Ready => {
            let payload: ReadyPayload = decode_payload(envelope)?;
            ensure(
                !payload.worker.is_empty() && !payload.capabilities.is_empty(),
                "ready requires worker and capabilities",
            )
        }
        Kind::Run => {
            let payload: RunPayload = decode_payload(envelope)?;
            ensure(
                !payload.args.is_empty() && !payload.working_directory.is_empty(),
                "run requires args and working_directory",
            )
        }
        Kind::Cancel => {
            let payload: CancelPayload = decode_payload(envelope)?;
            ensure(!payload.reason.is_empty(), "cancel requires reason")
        }
        Kind::Shutdown => {
            let _: ShutdownPayload = decode_payload(envelope)?;
            Ok(())
        }
        Kind::ShutdownAck => {
            let _: ShutdownPayload = decode_payload(envelope)?;
            Ok(())
        }
        Kind::Lifecycle => validate_lifecycle(envelope),
        Kind::Diagnostic => validate_diagnostic(envelope),
        Kind::Complete => validate_complete(envelope),
        Kind::Error => {
            let payload: ErrorPayload = decode_payload(envelope)?;
            ensure(
                !payload.code.is_empty() && !payload.message.is_empty(),
                "error requires code and message",
            )
        }
        Kind::Unknown => Err(ProtocolError::new(
            ErrorClass::UnknownKind,
            "unknown message kind",
        )),
    }
}

fn validate_lifecycle(envelope: &Envelope) -> Result<(), ProtocolError> {
    let payload: LifecyclePayload = decode_payload(envelope)?;
    ensure(
        !payload.phase.is_empty() && !payload.state.is_empty() && payload.elapsed_ns >= 0,
        "lifecycle requires phase, state, and non-negative elapsed_ns",
    )?;
    ensure(
        payload
            .metrics
            .iter()
            .all(|(name, value)| !name.is_empty() && *value >= 0),
        "lifecycle metrics must be named and non-negative",
    )
}

fn validate_diagnostic(envelope: &Envelope) -> Result<(), ProtocolError> {
    let payload: DiagnosticPayload = decode_payload(envelope)?;
    ensure(
        !payload.linter.is_empty()
            && !payload.message.is_empty()
            && !payload.path.is_empty()
            && payload.line > 0
            && payload.column > 0,
        "diagnostic requires source and positive position",
    )?;
    ensure(
        matches!(payload.severity.as_str(), "error" | "warning" | "info"),
        "unsupported diagnostic severity",
    )
}

fn validate_complete(envelope: &Envelope) -> Result<(), ProtocolError> {
    let payload: CompletePayload = decode_payload(envelope)?;
    ensure(
        (0..=255).contains(&payload.exit_code) && payload.issues >= 0 && payload.elapsed_ns >= 0,
        "complete values are outside their valid range",
    )
}

fn ensure(condition: bool, detail: &'static str) -> Result<(), ProtocolError> {
    if condition {
        Ok(())
    } else {
        Err(ProtocolError::new(ErrorClass::InvalidPayload, detail))
    }
}
