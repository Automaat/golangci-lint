package workerprotocol

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type invalidVector struct {
	Name           string     `json:"name"`
	ErrorClass     ErrorClass `json:"error_class"`
	Records        []string   `json:"records"`
	OversizedBytes int        `json:"oversized_bytes"`
	HexRecord      string     `json:"hex_record"`
	NestedArrays   int        `json:"nested_arrays"`
}

type normalizationVector struct {
	Name      string `json:"name"`
	Input     string `json:"input"`
	Canonical string `json:"canonical"`
}

func TestGoldenStreams(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join(testdataDir(), "*.jsonl"))
	require.NoError(t, err)
	require.NotEmpty(t, paths)

	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			file, openErr := os.Open(path)
			require.NoError(t, openErr)
			t.Cleanup(func() { require.NoError(t, file.Close()) })

			decoder := &Decoder{}
			scanner := bufio.NewScanner(file)
			for scanner.Scan() {
				line := append([]byte(nil), scanner.Bytes()...)
				envelope, decodeErr := decoder.DecodeLine(append(line, '\n'))
				require.NoError(t, decodeErr)

				encoded, encodeErr := Encode(envelope)
				require.NoError(t, encodeErr)
				assert.Equal(t, string(line)+"\n", string(encoded))
			}
			require.NoError(t, scanner.Err())
		})
	}
}

func TestInvalidGoldenVectors(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(testdataDir(), "invalid.json"))
	require.NoError(t, err)

	var vectors []invalidVector
	require.NoError(t, json.Unmarshal(data, &vectors))
	require.NotEmpty(t, vectors)

	for _, vector := range vectors {
		t.Run(vector.Name, func(t *testing.T) {
			decoder := &Decoder{}
			var decodeErr error
			for index, record := range vector.Records {
				_, decodeErr = decoder.DecodeLine([]byte(record + "\n"))
				if index < len(vector.Records)-1 {
					require.NoError(t, decodeErr, "setup record %d", index)
				}
			}
			if vector.OversizedBytes > 0 {
				_, decodeErr = decoder.DecodeLine([]byte(strings.Repeat("x", vector.OversizedBytes)))
			}
			if vector.HexRecord != "" {
				record, hexErr := hex.DecodeString(vector.HexRecord)
				require.NoError(t, hexErr)
				_, decodeErr = decoder.DecodeLine(record)
			}
			if vector.NestedArrays > 0 {
				const prefix = `{"protocol_version":1,"kind":"hello","payload":{"client":"controller","capabilities":["diagnostics"],"future":`
				build := func(depth int) string {
					return prefix + strings.Repeat("[", depth) + "0" + strings.Repeat("]", depth) + "}}"
				}
				_, boundaryErr := (&Decoder{}).DecodeLine([]byte(build(vector.NestedArrays - 1)))
				require.NoError(t, boundaryErr)
				record := build(vector.NestedArrays)
				_, decodeErr = decoder.DecodeLine([]byte(record))
			}

			require.Error(t, decodeErr)
			assert.Equal(t, vector.ErrorClass, ErrorClassOf(decodeErr))
		})
	}
}

func TestNormalizationGoldenVectors(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(testdataDir(), "normalization.json"))
	require.NoError(t, err)

	var vectors []normalizationVector
	require.NoError(t, json.Unmarshal(data, &vectors))
	require.NotEmpty(t, vectors)

	for _, vector := range vectors {
		t.Run(vector.Name, func(t *testing.T) {
			require.NotEqual(t, vector.Input, vector.Canonical)
			envelope, decodeErr := (&Decoder{}).DecodeLine([]byte(vector.Input))
			require.NoError(t, decodeErr)
			encoded, encodeErr := Encode(envelope)
			require.NoError(t, encodeErr)
			assert.Equal(t, vector.Canonical, string(encoded))
		})
	}
}

func TestNewEnvelope(t *testing.T) {
	envelope, err := NewEnvelope(KindError, "", 1, ErrorPayload{
		Code:    "bad_input",
		Message: "line\u2028paragraph\u2029end",
		Fatal:   false,
	})
	require.NoError(t, err)

	encoded, err := Encode(envelope)
	require.NoError(t, err)
	want := "{\"protocol_version\":1,\"kind\":\"error\",\"sequence\":1," +
		"\"payload\":{\"code\":\"bad_input\",\"message\":\"line\\u2028paragraph\\u2029end\",\"fatal\":false}}\n"
	if string(encoded) != want {
		t.Fatalf("encoded record mismatch:\nwant %s\n got %s", want, encoded)
	}
}

func TestMaximumRecordSize(t *testing.T) {
	const prefix = `{"protocol_version":1,"kind":"shutdown","payload":{"padding":"`
	const suffix = `"}}`
	record := prefix + strings.Repeat("x", MaxLineBytes-len(prefix)-len(suffix)) + suffix
	require.Len(t, record, MaxLineBytes)

	envelope, err := (&Decoder{}).DecodeLine([]byte(record + "\n"))
	require.NoError(t, err)
	encoded, err := Encode(envelope)
	require.NoError(t, err)
	assert.Equal(t, record+"\n", string(encoded))
}

func TestEscapedSeparatorAtMaximumSize(t *testing.T) {
	const prefix = `{"protocol_version":1,"kind":"error","sequence":1,"payload":{"code":"boundary","message":"`
	const suffix = `","fatal":true}}`
	build := func(padding int) Envelope {
		record := prefix + strings.Repeat("x", padding) + "\u2028" + suffix
		envelope, err := (&Decoder{}).DecodeLine([]byte(record + "\n"))
		require.NoError(t, err)

		return envelope
	}

	base, err := Encode(build(0))
	require.NoError(t, err)
	padding := MaxLineBytes - (len(base) - 1)

	encoded, err := Encode(build(padding))
	require.NoError(t, err)
	assert.Len(t, encoded, MaxLineBytes+1)

	_, err = Encode(build(padding + 1))
	require.Error(t, err)
	assert.Equal(t, ErrorTooLarge, ErrorClassOf(err))
}

func TestEncodeRejectsUnpairedSurrogate(t *testing.T) {
	envelope := Envelope{
		ProtocolVersion: Version,
		Kind:            KindHello,
		Payload: json.RawMessage(
			`{"client":"bad\uD800","capabilities":["diagnostics"]}`,
		),
	}

	_, err := Encode(envelope)
	require.Error(t, err)
	assert.Equal(t, ErrorInvalidEnvelope, ErrorClassOf(err))
}

func testdataDir() string {
	return filepath.Join("..", "..", "..", "testdata", "worker-protocol", "v1")
}
