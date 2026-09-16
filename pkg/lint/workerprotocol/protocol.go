package workerprotocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"unicode/utf8"
)

const (
	// Version is the only accepted wire protocol version.
	Version = 1
	// MaxLineBytes bounds one JSON object without its line terminator.
	MaxLineBytes = 1 << 20
	// MaxNestingDepth bounds nested JSON objects and arrays.
	MaxNestingDepth = 64
)

// Kind identifies an envelope payload.
type Kind string

const (
	KindHello      Kind = "hello"
	KindReady      Kind = "ready"
	KindRun        Kind = "run"
	KindCancel     Kind = "cancel"
	KindShutdown   Kind = "shutdown"
	KindLifecycle  Kind = "lifecycle"
	KindDiagnostic Kind = "diagnostic"
	KindComplete   Kind = "complete"
	KindError      Kind = "error"
)

// ErrorClass is a stable decode failure category.
type ErrorClass string

const (
	ErrorTooLarge           ErrorClass = "too_large"
	ErrorMalformedJSON      ErrorClass = "malformed_json"
	ErrorUnsupportedVersion ErrorClass = "unsupported_version"
	ErrorUnknownKind        ErrorClass = "unknown_kind"
	ErrorInvalidEnvelope    ErrorClass = "invalid_envelope"
	ErrorInvalidPayload     ErrorClass = "invalid_payload"
	ErrorInvalidSequence    ErrorClass = "invalid_sequence"
)

// ProtocolError reports a stable class and diagnostic detail.
type ProtocolError struct {
	Class ErrorClass
	Err   error
}

func (e *ProtocolError) Error() string {
	return fmt.Sprintf("worker protocol %s: %v", e.Class, e.Err)
}

func (e *ProtocolError) Unwrap() error {
	return e.Err
}

// Envelope is one protocol message.
type Envelope struct {
	ProtocolVersion int64           `json:"protocol_version"`
	Kind            Kind            `json:"kind"`
	RequestID       *string         `json:"request_id,omitempty"`
	Sequence        *uint64         `json:"sequence,omitempty"`
	Payload         json.RawMessage `json:"payload"`
}

// HelloPayload starts capability negotiation.
type HelloPayload struct {
	Client       string   `json:"client"`
	Capabilities []string `json:"capabilities"`
}

// ReadyPayload completes capability negotiation.
type ReadyPayload struct {
	Worker       string   `json:"worker"`
	Capabilities []string `json:"capabilities"`
}

// RunPayload describes one CLI-equivalent analysis.
type RunPayload struct {
	Args             []string          `json:"args"`
	WorkingDirectory string            `json:"working_directory"`
	Environment      map[string]string `json:"environment"`
}

// CancelPayload explains controller cancellation.
type CancelPayload struct {
	Reason string `json:"reason"`
}

// ShutdownPayload requests an orderly worker exit.
type ShutdownPayload struct{}

// LifecyclePayload reports one measured analysis phase.
type LifecyclePayload struct {
	Phase     string           `json:"phase"`
	State     string           `json:"state"`
	ElapsedNS int64            `json:"elapsed_ns"`
	Metrics   map[string]int64 `json:"metrics"`
	Error     string           `json:"error,omitempty"`
}

// DiagnosticPayload is one structured linter finding.
type DiagnosticPayload struct {
	Linter   string `json:"linter"`
	Message  string `json:"message"`
	Path     string `json:"path"`
	Line     int64  `json:"line"`
	Column   int64  `json:"column"`
	Severity string `json:"severity"`
}

// CompletePayload terminates one run request.
type CompletePayload struct {
	ExitCode     int64  `json:"exit_code"`
	Issues       int64  `json:"issues"`
	ElapsedNS    int64  `json:"elapsed_ns"`
	ContextError string `json:"context_error,omitempty"`
}

// ErrorPayload reports a worker or protocol failure.
type ErrorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Fatal   bool   `json:"fatal"`
}

// Decoder validates connection-wide event sequencing.
type Decoder struct {
	lastSequence uint64
}

// DecodeLine parses and validates one JSON-lines record.
func (d *Decoder) DecodeLine(line []byte) (Envelope, error) {
	record := trimTerminator(line)
	if len(record) > MaxLineBytes {
		return Envelope{}, protocolError(ErrorTooLarge, "record is %d bytes", len(record))
	}
	if !utf8.Valid(record) {
		return Envelope{}, protocolError(ErrorMalformedJSON, "record is not UTF-8")
	}
	if err := validateJSONStructure(record); err != nil {
		return Envelope{}, err
	}
	if err := validateEnvelopeFieldNames(record); err != nil {
		return Envelope{}, err
	}

	var envelope Envelope
	decoder := json.NewDecoder(bytes.NewReader(record))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return Envelope{}, decodeError(err)
	}
	if err := rejectTrailingJSON(decoder); err != nil {
		return Envelope{}, err
	}
	if hasUnpairedSurrogateEscape(record) {
		return Envelope{}, protocolError(ErrorInvalidEnvelope, "record contains an unpaired UTF-16 surrogate")
	}
	if err := requireJSONFields(record, ErrorInvalidEnvelope, false,
		"envelope", "protocol_version", "kind", "payload"); err != nil {
		return Envelope{}, err
	}
	if err := requireJSONFields(record, ErrorInvalidEnvelope, true,
		"envelope", "protocol_version", "kind"); err != nil {
		return Envelope{}, err
	}
	if err := validateEnvelope(envelope); err != nil {
		return Envelope{}, err
	}
	if envelope.Kind.isWorkerEvent() {
		sequence := *envelope.Sequence
		if sequence <= d.lastSequence {
			return Envelope{}, protocolError(ErrorInvalidSequence,
				"sequence %d does not follow %d", sequence, d.lastSequence)
		}
		d.lastSequence = sequence
	}

	return envelope, nil
}

// NewEnvelope marshals a typed payload and validates the message.
func NewEnvelope(kind Kind, requestID string, sequence uint64, payload any) (Envelope, error) {
	raw, err := marshalPayload(payload)
	if err != nil {
		return Envelope{}, protocolError(ErrorInvalidPayload, "marshal: %v", err)
	}

	envelope := Envelope{
		ProtocolVersion: Version,
		Kind:            kind,
		RequestID:       optionalString(requestID),
		Sequence:        optionalUint64(sequence),
		Payload:         raw,
	}
	if err := validateEnvelope(envelope); err != nil {
		return Envelope{}, err
	}

	return envelope, nil
}

// Encode emits one canonical newline-terminated record.
func Encode(envelope Envelope) ([]byte, error) {
	if err := validateEnvelope(envelope); err != nil {
		return nil, err
	}

	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(envelope); err != nil {
		return nil, protocolError(ErrorInvalidEnvelope, "encode: %v", err)
	}
	record := escapeLineSeparators(compactJSON(output.Bytes()))
	if err := validateJSONStructure(record); err != nil {
		return nil, err
	}
	if hasUnpairedSurrogateEscape(record) {
		return nil, protocolError(ErrorInvalidEnvelope, "record contains an unpaired UTF-16 surrogate")
	}
	if len(record) > MaxLineBytes {
		return nil, protocolError(ErrorTooLarge, "encoded record exceeds %d bytes", MaxLineBytes)
	}

	return append(record, '\n'), nil
}

// DecodePayload decodes known fields while allowing additive payload fields.
func DecodePayload[T any](envelope Envelope) (T, error) {
	var payload T
	data := envelope.Payload
	target := reflect.TypeFor[T]()
	if target.Kind() == reflect.Struct {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(data, &object); err != nil {
			return payload, protocolError(ErrorInvalidPayload, "decode %s payload: %v", envelope.Kind, err)
		}
		exact := make(map[string]json.RawMessage, target.NumField())
		for index := range target.NumField() {
			name, _, _ := strings.Cut(target.Field(index).Tag.Get("json"), ",")
			if value, ok := object[name]; ok && name != "" && name != "-" {
				exact[name] = value
			}
		}
		var err error
		data, err = json.Marshal(exact)
		if err != nil {
			return payload, protocolError(ErrorInvalidPayload, "filter %s payload: %v", envelope.Kind, err)
		}
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return payload, protocolError(ErrorInvalidPayload, "decode %s payload: %v", envelope.Kind, err)
	}

	return payload, nil
}

func validateEnvelopeFieldNames(record []byte) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(record, &object); err != nil {
		return nil
	}
	allowed := map[string]struct{}{
		"protocol_version": {},
		"kind":             {},
		"request_id":       {},
		"sequence":         {},
		"payload":          {},
	}
	for field := range object {
		if _, ok := allowed[field]; !ok {
			return protocolError(ErrorInvalidEnvelope, "unknown envelope field %q", field)
		}
	}

	return nil
}

func hasUnpairedSurrogateEscape(data []byte) bool {
	inString := false
	for index := 0; index < len(data); {
		value := data[index]
		if !inString {
			inString = value == '"'
			index++

			continue
		}
		if value == '"' {
			inString = false
			index++

			continue
		}
		if value != '\\' || index+1 >= len(data) {
			index++

			continue
		}
		if data[index+1] != 'u' {
			index += 2

			continue
		}

		width, invalid := surrogateEscapeWidth(data, index)
		if invalid {
			return true
		}
		index += width
	}

	return false
}

func surrogateEscapeWidth(data []byte, index int) (int, bool) {
	const (
		unicodeEscapeWidth       = 6
		surrogatePairEscapeWidth = 12
		lowSurrogateOffset       = 8
	)

	code, ok := decodeHexQuad(data, index+2)
	if !ok {
		return 2, false
	}
	if code >= 0xdc00 && code <= 0xdfff {
		return 0, true
	}
	if code < 0xd800 || code > 0xdbff {
		return unicodeEscapeWidth, false
	}
	if index+surrogatePairEscapeWidth > len(data) ||
		data[index+unicodeEscapeWidth] != '\\' ||
		data[index+unicodeEscapeWidth+1] != 'u' {
		return 0, true
	}
	low, lowOK := decodeHexQuad(data, index+lowSurrogateOffset)
	if !lowOK || low < 0xdc00 || low > 0xdfff {
		return 0, true
	}

	return surrogatePairEscapeWidth, false
}

func decodeHexQuad(data []byte, offset int) (uint16, bool) {
	if offset+4 > len(data) {
		return 0, false
	}

	var value uint16
	for _, digit := range data[offset : offset+4] {
		value <<= 4
		switch {
		case digit >= '0' && digit <= '9':
			value += uint16(digit - '0')
		case digit >= 'a' && digit <= 'f':
			value += uint16(digit-'a') + 10
		case digit >= 'A' && digit <= 'F':
			value += uint16(digit-'A') + 10
		default:
			return 0, false
		}
	}

	return value, true
}

func trimTerminator(line []byte) []byte {
	line = bytes.TrimSuffix(line, []byte{'\n'})

	return bytes.TrimSuffix(line, []byte{'\r'})
}

func rejectTrailingJSON(decoder *json.Decoder) error {
	var trailing any
	err := decoder.Decode(&trailing)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return protocolError(ErrorMalformedJSON, "multiple JSON values in one record")
	}

	return protocolError(ErrorMalformedJSON, "trailing JSON: %v", err)
}

type jsonStructureIssue int

const (
	jsonStructureOK jsonStructureIssue = iota
	jsonStructureDuplicate
	jsonStructureTooDeep
)

func validateJSONStructure(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()

	issue, err := scanJSONValue(decoder, 1)
	if err != nil {
		return nil
	}
	switch issue {
	case jsonStructureDuplicate:
		return protocolError(ErrorInvalidEnvelope, "record contains a duplicate object key")
	case jsonStructureTooDeep:
		return protocolError(ErrorInvalidEnvelope, "record exceeds nesting depth %d", MaxNestingDepth)
	default:
		return nil
	}
}

func scanJSONValue(decoder *json.Decoder, depth int) (jsonStructureIssue, error) {
	token, err := decoder.Token()
	if err != nil {
		return jsonStructureOK, err
	}

	delimiter, ok := token.(json.Delim)
	if !ok {
		return jsonStructureOK, nil
	}
	if depth > MaxNestingDepth {
		return jsonStructureTooDeep, nil
	}

	switch delimiter {
	case '{':
		seen := map[string]struct{}{}
		for decoder.More() {
			keyToken, keyErr := decoder.Token()
			if keyErr != nil {
				return jsonStructureOK, keyErr
			}
			key, keyOK := keyToken.(string)
			if !keyOK {
				return jsonStructureOK, protocolError(ErrorMalformedJSON, "object key is not a string")
			}
			if _, exists := seen[key]; exists {
				return jsonStructureDuplicate, nil
			}
			seen[key] = struct{}{}

			issue, valueErr := scanJSONValue(decoder, depth+1)
			if valueErr != nil || issue != jsonStructureOK {
				return issue, valueErr
			}
		}
	case '[':
		for decoder.More() {
			issue, valueErr := scanJSONValue(decoder, depth+1)
			if valueErr != nil || issue != jsonStructureOK {
				return issue, valueErr
			}
		}
	default:
		return jsonStructureOK, protocolError(ErrorMalformedJSON, "unexpected delimiter %q", delimiter)
	}

	_, err = decoder.Token()

	return jsonStructureOK, err
}

func decodeError(err error) error {
	var syntaxError *json.SyntaxError
	if errors.As(err, &syntaxError) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return protocolError(ErrorMalformedJSON, "%v", err)
	}

	return protocolError(ErrorInvalidEnvelope, "%v", err)
}

func validateEnvelope(envelope Envelope) error {
	if envelope.ProtocolVersion != Version {
		return protocolError(ErrorUnsupportedVersion, "got %d, want %d", envelope.ProtocolVersion, Version)
	}
	if !envelope.Kind.valid() {
		return protocolError(ErrorUnknownKind, "%q", envelope.Kind)
	}
	if err := validateRouting(envelope); err != nil {
		return err
	}

	return validatePayload(envelope)
}

func validateRouting(envelope Envelope) error {
	if envelope.RequestID != nil && *envelope.RequestID == "" {
		return protocolError(ErrorInvalidEnvelope, "%s forbids an empty request_id", envelope.Kind)
	}
	if envelope.Kind.runScoped() && envelope.RequestID == nil {
		return protocolError(ErrorInvalidEnvelope, "%s requires request_id", envelope.Kind)
	}
	if !envelope.Kind.runScoped() && envelope.Kind != KindError && envelope.RequestID != nil {
		return protocolError(ErrorInvalidEnvelope, "%s forbids request_id", envelope.Kind)
	}
	if envelope.Kind.isWorkerEvent() && (envelope.Sequence == nil || *envelope.Sequence == 0) {
		return protocolError(ErrorInvalidEnvelope, "%s requires a positive sequence", envelope.Kind)
	}
	if !envelope.Kind.isWorkerEvent() && envelope.Sequence != nil {
		return protocolError(ErrorInvalidEnvelope, "%s forbids sequence", envelope.Kind)
	}

	return nil
}

func validatePayload(envelope Envelope) error {
	trimmed := bytes.TrimSpace(envelope.Payload)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return protocolError(ErrorInvalidPayload, "%s payload must be an object", envelope.Kind)
	}
	if envelope.Kind.isWorkerEvent() {
		return validateWorkerPayload(envelope)
	}

	return validateControllerPayload(envelope)
}

func validateControllerPayload(envelope Envelope) error {
	switch envelope.Kind {
	case KindHello:
		return validateHello(envelope)
	case KindRun:
		return validateRun(envelope)
	case KindCancel:
		payload, err := decodeRequiredPayload[CancelPayload](envelope, "reason")
		if err != nil {
			return err
		}
		if payload.Reason == "" {
			return protocolError(ErrorInvalidPayload, "cancel requires reason")
		}
	case KindShutdown:
		_, err := DecodePayload[ShutdownPayload](envelope)
		return err
	}

	return nil
}

func validateHello(envelope Envelope) error {
	payload, err := decodeRequiredPayload[HelloPayload](envelope, "client", "capabilities")
	if err != nil {
		return err
	}
	if err := rejectNullArrayElements(envelope, "capabilities"); err != nil {
		return err
	}
	if payload.Client == "" || len(payload.Capabilities) == 0 {
		return protocolError(ErrorInvalidPayload, "hello requires client and capabilities")
	}

	return nil
}

func validateRun(envelope Envelope) error {
	payload, err := decodeRequiredPayload[RunPayload](envelope,
		"args", "working_directory", "environment")
	if err != nil {
		return err
	}
	if err := rejectNullArrayElements(envelope, "args"); err != nil {
		return err
	}
	if err := rejectNullMapValues(envelope, "environment"); err != nil {
		return err
	}
	if len(payload.Args) == 0 || payload.WorkingDirectory == "" || payload.Environment == nil {
		return protocolError(ErrorInvalidPayload, "run requires args and working_directory")
	}

	return nil
}

func validateWorkerPayload(envelope Envelope) error {
	switch envelope.Kind {
	case KindReady:
		payload, err := decodeRequiredPayload[ReadyPayload](envelope, "worker", "capabilities")
		if err != nil {
			return err
		}
		if err := rejectNullArrayElements(envelope, "capabilities"); err != nil {
			return err
		}
		if payload.Worker == "" || len(payload.Capabilities) == 0 {
			return protocolError(ErrorInvalidPayload, "ready requires worker and capabilities")
		}
	case KindLifecycle:
		return validateLifecycle(envelope)
	case KindDiagnostic:
		return validateDiagnostic(envelope)
	case KindComplete:
		return validateComplete(envelope)
	case KindError:
		payload, err := decodeRequiredPayload[ErrorPayload](envelope, "code", "message", "fatal")
		if err != nil {
			return err
		}
		if payload.Code == "" || payload.Message == "" {
			return protocolError(ErrorInvalidPayload, "error requires code and message")
		}
	}

	return nil
}

func validateLifecycle(envelope Envelope) error {
	payload, err := decodeRequiredPayload[LifecyclePayload](envelope,
		"phase", "state", "elapsed_ns", "metrics")
	if err != nil {
		return err
	}
	if err := rejectNullMapValues(envelope, "metrics"); err != nil {
		return err
	}
	if err := rejectNullOptionalField(envelope, "error"); err != nil {
		return err
	}
	if payload.Phase == "" || payload.State == "" || payload.ElapsedNS < 0 || payload.Metrics == nil {
		return protocolError(ErrorInvalidPayload, "lifecycle requires phase, state, and non-negative elapsed_ns")
	}
	for name, value := range payload.Metrics {
		if name == "" || value < 0 {
			return protocolError(ErrorInvalidPayload, "lifecycle metrics must be named and non-negative")
		}
	}

	return nil
}

func validateDiagnostic(envelope Envelope) error {
	payload, err := decodeRequiredPayload[DiagnosticPayload](envelope,
		"linter", "message", "path", "line", "column", "severity")
	if err != nil {
		return err
	}
	if payload.Linter == "" || payload.Message == "" || payload.Path == "" || payload.Line < 1 || payload.Column < 1 {
		return protocolError(ErrorInvalidPayload, "diagnostic requires source and positive position")
	}
	if payload.Severity != "error" && payload.Severity != "warning" && payload.Severity != "info" {
		return protocolError(ErrorInvalidPayload, "unsupported diagnostic severity %q", payload.Severity)
	}

	return nil
}

func validateComplete(envelope Envelope) error {
	payload, err := decodeRequiredPayload[CompletePayload](envelope,
		"exit_code", "issues", "elapsed_ns")
	if err != nil {
		return err
	}
	if err := rejectNullOptionalField(envelope, "context_error"); err != nil {
		return err
	}
	if payload.ExitCode < 0 || payload.ExitCode > 255 || payload.Issues < 0 || payload.ElapsedNS < 0 {
		return protocolError(ErrorInvalidPayload, "complete values are outside their valid range")
	}

	return nil
}

func (k Kind) valid() bool {
	switch k {
	case KindHello, KindReady, KindRun, KindCancel, KindShutdown,
		KindLifecycle, KindDiagnostic, KindComplete, KindError:
		return true
	default:
		return false
	}
}

func (k Kind) runScoped() bool {
	switch k {
	case KindRun, KindCancel, KindLifecycle, KindDiagnostic, KindComplete:
		return true
	default:
		return false
	}
}

func (k Kind) isWorkerEvent() bool {
	switch k {
	case KindReady, KindLifecycle, KindDiagnostic, KindComplete, KindError:
		return true
	default:
		return false
	}
}

func protocolError(class ErrorClass, format string, args ...any) error {
	return &ProtocolError{Class: class, Err: fmt.Errorf(format, args...)}
}

func decodeRequiredPayload[T any](envelope Envelope, fields ...string) (T, error) {
	if err := requireJSONFields(envelope.Payload, ErrorInvalidPayload, true,
		string(envelope.Kind), fields...); err != nil {
		var zero T

		return zero, err
	}

	return DecodePayload[T](envelope)
}

func requireJSONFields(data []byte, class ErrorClass, rejectNull bool, subject string, fields ...string) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return protocolError(class, "decode %s: %v", subject, err)
	}
	for _, field := range fields {
		value, ok := object[field]
		if !ok || rejectNull && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return protocolError(class, "%s requires %s", subject, field)
		}
	}

	return nil
}

func rejectNullArrayElements(envelope Envelope, field string) error {
	raw, err := payloadField(envelope, field)
	if err != nil {
		return err
	}

	var elements []json.RawMessage
	if err := json.Unmarshal(raw, &elements); err != nil {
		return protocolError(ErrorInvalidPayload, "%s.%s: %v", envelope.Kind, field, err)
	}
	for _, element := range elements {
		if isJSONNull(element) {
			return protocolError(ErrorInvalidPayload, "%s.%s forbids null elements", envelope.Kind, field)
		}
	}

	return nil
}

func rejectNullMapValues(envelope Envelope, field string) error {
	raw, err := payloadField(envelope, field)
	if err != nil {
		return err
	}

	var values map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return protocolError(ErrorInvalidPayload, "%s.%s: %v", envelope.Kind, field, err)
	}
	for _, value := range values {
		if isJSONNull(value) {
			return protocolError(ErrorInvalidPayload, "%s.%s forbids null values", envelope.Kind, field)
		}
	}

	return nil
}

func rejectNullOptionalField(envelope Envelope, field string) error {
	raw, err := payloadField(envelope, field)
	if err != nil {
		return err
	}
	if raw != nil && isJSONNull(raw) {
		return protocolError(ErrorInvalidPayload, "%s.%s forbids null", envelope.Kind, field)
	}

	return nil
}

func payloadField(envelope Envelope, field string) (json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(envelope.Payload, &object); err != nil {
		return nil, protocolError(ErrorInvalidPayload, "decode %s payload: %v", envelope.Kind, err)
	}

	return object[field], nil
}

func isJSONNull(value []byte) bool {
	return bytes.Equal(bytes.TrimSpace(value), []byte("null"))
}

// ErrorClassOf extracts a stable class from a protocol error.
func ErrorClassOf(err error) ErrorClass {
	var protocolErr *ProtocolError
	if errors.As(err, &protocolErr) {
		return protocolErr.Class
	}

	return ""
}

func marshalPayload(payload any) ([]byte, error) {
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(payload); err != nil {
		return nil, err
	}

	return bytes.TrimSuffix(output.Bytes(), []byte{'\n'}), nil
}

func escapeLineSeparators(record []byte) []byte {
	record = bytes.ReplaceAll(record, []byte("\u2028"), []byte(`\u2028`))

	return bytes.ReplaceAll(record, []byte("\u2029"), []byte(`\u2029`))
}

func compactJSON(record []byte) []byte {
	compacted := make([]byte, 0, len(record))
	inString := false
	escaped := false
	for _, value := range record {
		if inString {
			compacted = append(compacted, value)
			if escaped {
				escaped = false
			} else if value == '\\' {
				escaped = true
			} else if value == '"' {
				inString = false
			}

			continue
		}
		if value == '"' {
			inString = true
			compacted = append(compacted, value)

			continue
		}
		if value != ' ' && value != '\t' && value != '\r' && value != '\n' {
			compacted = append(compacted, value)
		}
	}

	return compacted
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}

	return &value
}

func optionalUint64(value uint64) *uint64 {
	if value == 0 {
		return nil
	}

	return &value
}
