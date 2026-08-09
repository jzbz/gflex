package bootloader

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// page builds a deterministic 8*n byte page.
func page(seed byte, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = seed + byte(i)
	}
	return b
}

func numberArray(b []byte) string {
	parts := make([]string, len(b))
	for i, v := range b {
		parts[i] = fmt.Sprint(v)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// The three page encodings the vendor payload has been seen to use, across the
// three key names it has been seen to use (SPEC.md §10.3).
func TestParseJSONPayloadEncodings(t *testing.T) {
	t.Parallel()
	p0, p1 := page(0x10, 16), page(0x80, 16)

	tests := []struct {
		name string
		body string
	}{
		{
			name: "app_bin, arrays of numbers",
			body: fmt.Sprintf(`{"app_bin":[%s,%s],"app_version":"5.1.0","crc":90}`,
				numberArray(p0), numberArray(p1)),
		},
		{
			name: "app_bin_data, base64 strings",
			body: fmt.Sprintf(`{"app_bin_data":["%s","%s"],"app_version":"5.1.0","crc":90}`,
				base64.StdEncoding.EncodeToString(p0), base64.StdEncoding.EncodeToString(p1)),
		},
		{
			name: "firmware, hex strings",
			body: fmt.Sprintf(`{"firmware":["%s","%s"],"app_version":"5.1.0","crc":90}`,
				hex.EncodeToString(p0), hex.EncodeToString(p1)),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fw, err := ParseJSONPayload([]byte(tc.body))
			if err != nil {
				t.Fatalf("ParseJSONPayload: %v", err)
			}
			if len(fw.Pages) != 2 {
				t.Fatalf("got %d pages, want 2", len(fw.Pages))
			}
			if !bytes.Equal(fw.Pages[0], p0) || !bytes.Equal(fw.Pages[1], p1) {
				t.Errorf("page bytes differ:\n got %x %x\nwant %x %x",
					fw.Pages[0], fw.Pages[1], p0, p1)
			}
			if fw.Version != "5.1.0" {
				t.Errorf("Version = %q, want 5.1.0", fw.Version)
			}
			if !fw.CRCKnown || fw.CRC != 90 {
				t.Errorf("CRC = 0x%02x known=%v, want 90 known=true", fw.CRC, fw.CRCKnown)
			}
			if fw.PageSize() != 16 || fw.ChunkSize() != 2 {
				t.Errorf("PageSize/ChunkSize = %d/%d, want 16/2", fw.PageSize(), fw.ChunkSize())
			}
		})
	}
}

// A hex string that is also valid base64 must decode as hex: real firmware
// base64 is essentially never all hex digits, while hex dumps always are.
func TestDecodeByteStringHexWinsAmbiguity(t *testing.T) {
	t.Parallel()
	got, err := decodeByteString("abcdef")
	if err != nil {
		t.Fatalf("decodeByteString: %v", err)
	}
	if want := []byte{0xAB, 0xCD, 0xEF}; !bytes.Equal(got, want) {
		t.Errorf("got % x, want % x", got, want)
	}
}

func TestDecodeByteStringWhitespaceAndPrefix(t *testing.T) {
	t.Parallel()
	got, err := decodeByteString(" 0x01 02\n03 04 ")
	if err != nil {
		t.Fatalf("decodeByteString: %v", err)
	}
	if want := []byte{1, 2, 3, 4}; !bytes.Equal(got, want) {
		t.Errorf("got % x, want % x", got, want)
	}
}

func TestParseJSONPayloadCRCForms(t *testing.T) {
	t.Parallel()
	pg := numberArray(page(0, 8))
	tests := []struct {
		name     string
		crcField string
		want     uint8
		known    bool
	}{
		{"number", `,"crc":31`, 31, true},
		{"decimal string", `,"crc":"31"`, 31, true},
		{"hex string", `,"crc":"0x1f"`, 0x1F, true},
		{"absent", ``, 0, false},
		{"null", `,"crc":null`, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"app_bin":[%s]%s}`, pg, tc.crcField)
			fw, err := ParseJSONPayload([]byte(body))
			if err != nil {
				t.Fatalf("ParseJSONPayload: %v", err)
			}
			if fw.CRCKnown != tc.known || fw.CRC != tc.want {
				t.Errorf("CRC = 0x%02x known=%v, want 0x%02x known=%v",
					fw.CRC, fw.CRCKnown, tc.want, tc.known)
			}
		})
	}
}

func TestParseJSONPayloadErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "page not divisible by eight",
			body: fmt.Sprintf(`{"app_bin":[%s]}`, numberArray(page(0, 12))),
			want: "data length 12 not divisible by 8",
		},
		{
			name: "unequal pages",
			body: fmt.Sprintf(`{"app_bin":[%s,%s]}`, numberArray(page(0, 16)), numberArray(page(0, 8))),
			want: "page 1 is 8 bytes but page 0 is 16",
		},
		{
			name: "no pages key",
			body: `{"app_version":"5.1.0","crc":1}`,
			want: "none of app_bin, app_bin_data or firmware is present",
		},
		{
			name: "empty page list",
			body: `{"app_bin":[]}`,
			want: "no pages",
		},
		{
			name: "byte out of range",
			body: `{"app_bin":[[1,2,3,4,5,6,7,256]]}`,
			want: "is not in 0-255",
		},
		{
			name: "crc out of range",
			body: fmt.Sprintf(`{"app_bin":[%s],"crc":256}`, numberArray(page(0, 8))),
			want: "not an 8-bit value",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseJSONPayload([]byte(tc.body))
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tc.want)
			}
		})
	}
}

// A flat array of numbers, or a single string, is the whole image rather than a
// list of pages; the page geometry then has to be imposed.
func TestParseJSONPayloadFlatImage(t *testing.T) {
	t.Parallel()
	img := page(0, 48)
	body := fmt.Sprintf(`{"app_bin":%s,"page_size":16,"crc":7}`, numberArray(img))
	fw, err := ParseJSONPayload([]byte(body))
	if err != nil {
		t.Fatalf("ParseJSONPayload: %v", err)
	}
	if len(fw.Pages) != 3 || fw.PageSize() != 16 {
		t.Fatalf("got %d pages of %d bytes, want 3 of 16", len(fw.Pages), fw.PageSize())
	}
	if !bytes.Equal(bytes.Join(fw.Pages, nil), img) {
		t.Error("reassembled image differs from the input")
	}
}

func TestParseImageRawBinary(t *testing.T) {
	t.Parallel()
	img := page(1, 1024)
	fw, err := ParseImage(img, LoadOptions{})
	if err != nil {
		t.Fatalf("ParseImage: %v", err)
	}
	if fw.PageSize() != DefaultPageSize {
		t.Errorf("PageSize = %d, want %d", fw.PageSize(), DefaultPageSize)
	}
	if len(fw.Pages) != 2 {
		t.Errorf("got %d pages, want 2", len(fw.Pages))
	}
	// A raw image carries no CRC, so verification cannot be checked.
	if fw.CRCKnown {
		t.Error("CRCKnown = true for a raw binary, want false")
	}
	if fw.CRC != 0 {
		t.Errorf("CRC = 0x%02x, want 0", fw.CRC)
	}
}

func TestParseImageRawBinaryPadsFinalPage(t *testing.T) {
	t.Parallel()
	img := page(1, 20)
	fw, err := ParseImage(img, LoadOptions{PageSize: 16})
	if err != nil {
		t.Fatalf("ParseImage: %v", err)
	}
	if len(fw.Pages) != 2 {
		t.Fatalf("got %d pages, want 2", len(fw.Pages))
	}
	if !bytes.Equal(fw.Pages[1][:4], img[16:]) {
		t.Errorf("tail = % x, want % x", fw.Pages[1][:4], img[16:])
	}
	if want := bytes.Repeat([]byte{RawImagePad}, 12); !bytes.Equal(fw.Pages[1][4:], want) {
		t.Errorf("padding = % x, want % x", fw.Pages[1][4:], want)
	}
}

func TestParseImageRawBinaryBadPageSize(t *testing.T) {
	t.Parallel()
	_, err := ParseImage(page(0, 100), LoadOptions{PageSize: 100})
	if err == nil {
		t.Fatal("expected an error for a page size not divisible by 8")
	}
	if want := "data length 100 not divisible by 8"; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to contain %q", err.Error(), want)
	}
}

func TestLoadFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	binPath := filepath.Join(dir, "image.bin")
	if err := os.WriteFile(binPath, page(3, 512), 0o600); err != nil {
		t.Fatal(err)
	}
	fw, err := LoadFile(binPath)
	if err != nil {
		t.Fatalf("LoadFile(bin): %v", err)
	}
	if len(fw.Pages) != 1 || fw.CRCKnown {
		t.Errorf("bin: %d pages, CRCKnown=%v", len(fw.Pages), fw.CRCKnown)
	}

	jsonPath := filepath.Join(dir, "image.json")
	body := fmt.Sprintf("\n  {\"firmware\":[%s],\"app_version\":\" 5.2.0 \",\"crc\":18}",
		numberArray(page(0, 64)))
	if err := os.WriteFile(jsonPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	fw, err = LoadFile(jsonPath)
	if err != nil {
		t.Fatalf("LoadFile(json): %v", err)
	}
	if fw.Version != "5.2.0" {
		t.Errorf("Version = %q, want 5.2.0", fw.Version)
	}
	if !fw.CRCKnown || fw.CRC != 18 {
		t.Errorf("CRC = 0x%02x known=%v, want 18 known=true", fw.CRC, fw.CRCKnown)
	}
	if fw.ChunkSize() != 8 {
		t.Errorf("ChunkSize = %d, want 8", fw.ChunkSize())
	}

	if _, err := LoadFile(filepath.Join(dir, "missing.bin")); err == nil {
		t.Error("expected an error for a missing file")
	}
}

func TestLoadFileWithOptionsPageSize(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "image.bin")
	if err := os.WriteFile(path, page(0, 256), 0o600); err != nil {
		t.Fatal(err)
	}
	fw, err := LoadFileWithOptions(path, LoadOptions{PageSize: 128})
	if err != nil {
		t.Fatalf("LoadFileWithOptions: %v", err)
	}
	if fw.PageSize() != 128 || len(fw.Pages) != 2 {
		t.Errorf("got %d pages of %d bytes, want 2 of 128", len(fw.Pages), fw.PageSize())
	}
}

func TestFirmwareValidate(t *testing.T) {
	t.Parallel()
	var nilFW *Firmware
	if err := nilFW.Validate(); err == nil {
		t.Error("expected an error for a nil image")
	}
	if err := (&Firmware{}).Validate(); err == nil {
		t.Error("expected an error for an image with no pages")
	}
	// A page size that would overflow a single WRITE_CHUNK frame.
	big := &Firmware{Pages: [][]byte{make([]byte, (MaxChunkSize+1)*ChunksPerPage)}}
	if err := big.Validate(); err == nil {
		t.Error("expected an error for a page that yields oversize chunks")
	}
	ok := &Firmware{Pages: [][]byte{make([]byte, 64), make([]byte, 64)}}
	if err := ok.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

// The WRITE_CHUNK page id is a big-endian uint16, so page MaxPages would wrap
// to page 0 and rewrite the start of an image the device has already committed.
// It has to be caught here, before the first write: nothing downstream would
// notice, and the device's CRC is computed over whatever it was told to write.
func TestFirmwareValidateRejectsMorePagesThanTheIDField(t *testing.T) {
	t.Parallel()
	pg := make([]byte, 8)
	pages := make([][]byte, MaxPages+1)
	for i := range pages {
		pages[i] = pg
	}
	err := (&Firmware{Pages: pages}).Validate()
	if err == nil {
		t.Fatal("expected an image with more than MaxPages pages to be rejected")
	}
	if want := "16-bit"; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to explain the %s page id", err.Error(), want)
	}
	// The boundary itself is legal.
	if err := (&Firmware{Pages: pages[:MaxPages]}).Validate(); err != nil {
		t.Errorf("Validate(%d pages): %v", MaxPages, err)
	}
}
