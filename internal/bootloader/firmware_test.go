package bootloader

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jzbz/gflex/internal/proto"
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

// An all-hex string IS hex, and an odd length then means damage — most likely
// truncation. It must error, never fall through to base64: hex digits are all
// valid base64, so a hex image truncated to length 4k+3 used to decode via
// RawStdEncoding into unrelated garbage, which the no-CRC --force path would
// then flash.
func TestDecodeByteStringTruncatedHexErrors(t *testing.T) {
	t.Parallel()
	for _, s := range []string{
		"0123456789abcde", // 15 digits: 4k+3, valid raw base64 — the trap
		"abcde",           // 5 digits: 4k+1, base64 happened to reject it too
	} {
		_, err := decodeByteString(s)
		if err == nil {
			t.Fatalf("decodeByteString(%q) decoded a truncated hex image", s)
		}
		if !strings.Contains(err.Error(), "truncated") || !strings.Contains(err.Error(), "hex") {
			t.Errorf("decodeByteString(%q) error = %v; want it to name odd-length hex and likely truncation", s, err)
		}
	}
}

// When a string is not hex and not base64 either, the error reports both
// interpretations — naming the character that ruled hex out, so a corrupted
// hex dump is diagnosable as one.
func TestDecodeByteStringCorruptCharNamesBothInterpretations(t *testing.T) {
	t.Parallel()
	_, err := decodeByteString("0123%567")
	if err == nil {
		t.Fatal("decodeByteString decoded a corrupt string")
	}
	for _, want := range []string{"hex", "base64", "'%'"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to mention %q", err, want)
		}
	}
}

// Genuine base64 — anything containing a non-hex character such as padding,
// '+', or letters past 'f' — still decodes through the base64 alphabets.
func TestDecodeByteStringBase64StillDecodes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want []byte
	}{
		{base64.StdEncoding.EncodeToString([]byte{1, 2, 3, 4}), []byte{1, 2, 3, 4}}, // "AQIDBA==": '=' padding
		{"+/+/", []byte{0xFB, 0xFF, 0xBF}},                                          // std alphabet specials
		{"-_-_", []byte{0xFB, 0xFF, 0xBF}},                                          // URL-safe alphabet
	}
	for _, tc := range tests {
		got, err := decodeByteString(tc.in)
		if err != nil {
			t.Fatalf("decodeByteString(%q): %v", tc.in, err)
		}
		if !bytes.Equal(got, tc.want) {
			t.Errorf("decodeByteString(%q) = % x, want % x", tc.in, got, tc.want)
		}
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

// "0x" is a hex marker only when what follows is hex. Both characters are
// members of every base64 alphabet, so stripping the prefix before deciding the
// encoding truncated any base64 page whose encoding happens to begin with them
// -- about one page in 4096 -- and returned bytes offset from the real image by
// a byte and a half. A flat image has no per-page equality constraint to trip
// over it, so on the no-CRC --force path that garbage would be flashed.
func TestDecodeByteStringBase64BeginningWithZeroX(t *testing.T) {
	t.Parallel()
	// 0xD3 0x1n is the family whose base64 starts "0x": the first six bits are
	// 52 ('0') and the next six are 49 ('x'). Nine bytes, so the encoding has no
	// padding and the old prefix strip left a string RawStdEncoding accepts --
	// the silent-corruption case rather than the loud one.
	img := []byte{0xD3, 0x10, 0x41, 0x42, 0x43, 0x44, 0x45, 0x46, 0x47}
	s := base64.StdEncoding.EncodeToString(img)
	if !strings.HasPrefix(s, "0x") {
		t.Fatalf("base64 of % x is %q, which does not begin \"0x\"; the test no longer covers the case", img, s)
	}
	got, err := decodeByteString(s)
	if err != nil {
		t.Fatalf("decodeByteString(%q): %v", s, err)
	}
	if !bytes.Equal(got, img) {
		t.Errorf("decodeByteString(%q) = % x, want % x", s, got, img)
	}
}

// A key written as null is present as far as encoding/json is concerned: it
// stores the four bytes "null" in the json.RawMessage, so a length test alone
// let it stop the three-key fallback (SPEC.md §10.3) at the first key and
// report "no pages" about a payload whose pages sit under the second. Any
// serializer that renders unused optional fields rather than omitting them
// produces exactly this.
func TestParseJSONPayloadNullKeyFallsThrough(t *testing.T) {
	t.Parallel()
	pg := page(0x10, 16)
	tests := []struct {
		name string
		body string
	}{
		{
			name: "null app_bin, pages under app_bin_data",
			body: fmt.Sprintf(`{"app_bin":null,"app_bin_data":[%s],"crc":90}`, numberArray(pg)),
		},
		{
			name: "null app_bin and app_bin_data, pages under firmware",
			body: fmt.Sprintf(`{"app_bin":null,"app_bin_data":null,"firmware":[%s],"crc":90}`, numberArray(pg)),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fw, err := ParseJSONPayload([]byte(tc.body))
			if err != nil {
				t.Fatalf("ParseJSONPayload: %v", err)
			}
			if len(fw.Pages) != 1 || !bytes.Equal(fw.Pages[0], pg) {
				t.Errorf("got %d pages (%x), want one page of % x", len(fw.Pages), fw.Pages, pg)
			}
		})
	}

	// A payload whose only pages key is null is still an error, and it is the
	// accurate one rather than "has no pages".
	_, err := ParseJSONPayload([]byte(`{"app_bin":null,"crc":90}`))
	if err == nil {
		t.Fatal("expected an error when every pages key is null")
	}
	if want := "none of app_bin, app_bin_data or firmware is present"; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to contain %q", err.Error(), want)
	}
}

// The page ceiling has to be applied to the element count before the pages are
// decoded, not only in newFirmware afterwards. The document is bounded by
// MaxMessageBytes; the number of elements inside it is not, and two bytes of
// "[]," per element used to buy a 24-byte slice header here and another in the
// decoded slice -- a few hundred megabytes and millions of decode calls from an
// 8 MiB message, all of it discarded.
//
// The elements below are individually undecodable, so an implementation that
// still decodes first reports the page rather than the count: that is what
// makes this test fail when the early ceiling is removed.
func TestNormalisePagesBoundsPageCountBeforeDecoding(t *testing.T) {
	t.Parallel()
	var sb strings.Builder
	sb.WriteString(`{"app_bin":[`)
	for i := 0; i <= MaxPages; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`"!"`) // neither hex nor base64 in any alphabet
	}
	sb.WriteString(`]}`)

	_, err := ParseJSONPayload([]byte(sb.String()))
	if err == nil {
		t.Fatal("expected an image with more than MaxPages pages to be rejected")
	}
	if !errors.Is(err, ErrBadPageLength) {
		t.Errorf("error = %v, want it to wrap ErrBadPageLength", err)
	}
	if want := "16-bit"; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want the page count refused before any page is decoded (%q)", err.Error(), want)
	}
	// The count is refused the moment it is exceeded, so the message cannot
	// name the total: a document with far more elements than the id field can
	// address must not be walked to the end just to put a number in an error.
	if want := "more than"; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to say %q rather than a total that only a full walk could know",
			err.Error(), want)
	}
}

// The same ceiling, on a document that carries far more elements than it: an
// implementation that unmarshals the array whole before counting pays 24 bytes
// of slice header for every three bytes of "[]," -- 31x, measured, and 2.37 GB
// from a 75 MB page array -- and it can name the total, which is exactly what
// this asserts it cannot do.
func TestNormalisePagesStopsCountingAtTheCeiling(t *testing.T) {
	t.Parallel()
	const excess = 50000
	var sb strings.Builder
	sb.Grow(3*(MaxPages+excess) + 32)
	sb.WriteString(`{"app_bin":[`)
	for i := 0; i < MaxPages+excess; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`[]`)
	}
	sb.WriteString(`]}`)

	_, err := ParseJSONPayload([]byte(sb.String()))
	if err == nil {
		t.Fatal("expected an image with more than MaxPages pages to be rejected")
	}
	if !errors.Is(err, ErrBadPageLength) {
		t.Errorf("error = %v, want it to wrap ErrBadPageLength", err)
	}
	if got := fmt.Sprint(MaxPages + excess); strings.Contains(err.Error(), got) {
		t.Errorf("error = %q names the total element count %s, so the whole array was decoded before it was judged",
			err.Error(), got)
	}
	if want := fmt.Sprintf("more than %d pages", MaxPages); !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to contain %q", err.Error(), want)
	}
}

// A flat array of byte values has no page ceiling to bound it -- its elements
// are bytes, and an image of more than MaxPages bytes is perfectly ordinary --
// so it needs a ceiling of its own: the largest image the geometry can address,
// MaxPages pages of pageSize bytes. It has to be applied while the array is
// decoded rather than to the finished bytes.
//
// Measured before it existed: a 140 MB flat array cost 3.39 GB, 24x the file,
// because encoding/json materialises a json.RawMessage per element and then a
// json.Number per element before any value is looked at. That makes a ~300 MB
// image an out-of-memory kill on an 8 GB host rather than the error it deserves,
// and `gflex firmware flash <path> --dry-run` reaches this loader with no
// hardware attached at all.
//
// page_size 8 -- the smallest legal geometry -- is what keeps these documents
// small: about a megabyte for the ceiling itself and four for the one that
// exceeds it. The rule under test is MaxPages*pageSize bytes, not a constant.
//
// Not parallel: it reads process-global allocation counters, and top-level
// parallel tests are held until every sequential one has finished.
func TestNormalisePagesBoundsFlatImageBeforeDecoding(t *testing.T) {
	const pageSize = ChunksPerPage
	const ceiling = MaxPages * pageSize

	doc := func(n int) []byte {
		var b bytes.Buffer
		b.Grow(2*n + 40)
		fmt.Fprintf(&b, `{"page_size":%d,"app_bin":[`, pageSize)
		// Written without a per-element loop: the over-limit document below is
		// millions of elements, and under -race the loop costs more than the
		// parse being measured.
		b.Write(bytes.Repeat([]byte("0,"), n-1))
		b.WriteString(`0]}`)
		return b.Bytes()
	}

	// Four times the ceiling, not one element over it. A decoder with no
	// ceiling at all stops in the same place on a ceiling+1 document and
	// allocates the same -- measured identical to the byte -- so the budget
	// below would be pinning the streaming rewrite rather than the ceiling, and
	// only the error message would have anything to say.
	over := doc(4 * ceiling)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := ParseJSONPayload(over)
	runtime.ReadMemStats(&after)

	if err == nil {
		t.Fatal("expected a flat image larger than the page id can address to be rejected")
	}
	if !errors.Is(err, ErrBadPageLength) {
		t.Errorf("error = %v, want it to wrap ErrBadPageLength", err)
	}
	if want := fmt.Sprint(ceiling); !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want the %d-byte ceiling to be what refused it (%q); reporting the page count "+
			"instead means the whole array was decoded and split first", err.Error(), ceiling, want)
	}

	// TotalAlloc is cumulative, so it does not depend on when the collector
	// ran; what it measures is everything the parse asked for. Measured on this
	// 4.2 MB document: 179 MB before the bound existed -- 43x -- against 13 MB
	// with it. The budget sits between them with about 1.8x of room on each
	// side, because what is being pinned is the amplification, tens of bytes
	// per element, and not an exact figure.
	//
	// The room on the low side has to be that wide because CI runs `go test
	// -race`, and the race detector inflates this path unevenly: it takes the
	// bounded decode from 13 MB to 55 MB, since that is per-element work inside
	// the loop, while leaving the 179 MB of a whole-array json.Unmarshal
	// untouched. A budget picked without -race in hand lands between the two.
	const budget = 96 << 20
	if got := after.TotalAlloc - before.TotalAlloc; got > budget {
		t.Errorf("parsing a %d-byte document allocated %d bytes, over the %d-byte budget: the array was "+
			"materialised before the ceiling was applied", len(over), got, budget)
	}

	// The ceiling itself is a legal image -- exactly MaxPages pages -- and a
	// bound that rejects it would make a valid image unflashable.
	fw, err := ParseJSONPayload(doc(ceiling))
	if err != nil {
		t.Fatalf("ParseJSONPayload(%d bytes, the ceiling): %v", ceiling, err)
	}
	if len(fw.Pages) != MaxPages || fw.PageSize() != pageSize {
		t.Errorf("got %d pages of %d bytes, want %d of %d", len(fw.Pages), fw.PageSize(), MaxPages, pageSize)
	}
}

// One page element is bounded too, by the largest page the wire format can
// express. The page ceiling above does not reach it: a single element carrying
// a hundred megabytes of byte values is one page, and newFirmware would reject
// it for its length -- after every one of those values had been decoded.
func TestDecodePageBoundsOnePageBeforeDecoding(t *testing.T) {
	t.Parallel()
	var sb strings.Builder
	sb.WriteString(`{"app_bin":[[`)
	// A multiple of ChunksPerPage, so that an implementation which decodes
	// first fails on the chunk size rather than on divisibility.
	for i := 0; i < maxPageSize+ChunksPerPage; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteByte('0')
	}
	sb.WriteString(`]]}`)

	_, err := ParseJSONPayload([]byte(sb.String()))
	if err == nil {
		t.Fatal("expected a page longer than the wire format can express to be rejected")
	}
	if !errors.Is(err, ErrBadPageLength) {
		t.Errorf("error = %v, want it to wrap ErrBadPageLength", err)
	}
	if want := fmt.Sprintf("longer than the %d bytes", maxPageSize); !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want the %d-byte ceiling to be what refused it (%q)", err.Error(), maxPageSize, want)
	}
	if want := "(page 0)"; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to name %q", err.Error(), want)
	}
}

// The raw path judges its page count before it builds the pages. newFirmware
// refuses this image either way, but only after every page of it has been
// allocated and copied: a 500 MB .bin peaked at 1.08 GB to be told it has
// 976563 pages.
//
// Not parallel, for the same reason as the flat-image test above.
func TestParseRawImageBoundsPageCountBeforeSplitting(t *testing.T) {
	const pageSize = ChunksPerPage
	data := make([]byte, MaxPages*pageSize+1)

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := ParseImage(data, LoadOptions{PageSize: pageSize})
	runtime.ReadMemStats(&after)

	if err == nil {
		t.Fatal("expected an image with more pages than the page id can address to be rejected")
	}
	if !errors.Is(err, ErrBadPageLength) {
		t.Errorf("error = %v, want it to wrap ErrBadPageLength", err)
	}
	// newFirmware's wording, so the same violation reads the same way wherever
	// it is caught, and the count is exact here because it is arithmetic.
	if want := fmt.Sprintf("image has %d pages", MaxPages+1); !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to contain %q", err.Error(), want)
	}
	if want := "16-bit"; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to explain the %s page id", err.Error(), want)
	}
	// Nothing at all should have been built: the input is already in memory and
	// the count is a division. Before the check the split cost about 3.6 MB for
	// this 512 KiB image -- a slice header and a page per 8 bytes.
	const budget = 64 << 10
	if got := after.TotalAlloc - before.TotalAlloc; got > budget {
		t.Errorf("refusing a %d-byte image allocated %d bytes, over the %d-byte budget: the pages were built "+
			"before the count was judged", len(data), got, budget)
	}

	// The ceiling itself still splits.
	fw, err := ParseImage(data[:MaxPages*pageSize], LoadOptions{PageSize: pageSize})
	if err != nil {
		t.Fatalf("ParseImage(%d pages): %v", MaxPages, err)
	}
	if len(fw.Pages) != MaxPages {
		t.Errorf("got %d pages, want %d", len(fw.Pages), MaxPages)
	}
}

// A bare array is read from a file, not from a payload that has already been
// parsed once, so the array decode is the only thing standing between a
// malformed document and a firmware image. Trailing data and a truncated array
// both used to be caught by unmarshalling the whole document; reading the
// elements one at a time does not get that for free.
func TestParseImageRejectsMalformedBareArray(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, doc string
	}{
		{"trailing text", "[1,2,3,4,5,6,7,8] and then some"},
		{"a second value", "[1,2,3,4,5,6,7,8][9]"},
		{"truncated", "[1,2,3,4,5,6,7,8"},
		{"truncated pages", `[[1,2,3,4,5,6,7,8]`},
		{"trailing text after pages", `[[1,2,3,4,5,6,7,8]] and then some`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if fw, err := ParseImage([]byte(tc.doc), LoadOptions{}); err == nil {
				t.Errorf("accepted %q as %d pages of %d bytes", tc.doc, len(fw.Pages), fw.PageSize())
			} else if !errors.Is(err, ErrBadPageLength) {
				t.Errorf("error = %v, want it to wrap ErrBadPageLength", err)
			}
		})
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

	// A bare JSON array of byte values is the one JSON shape with no page split
	// of its own, so it is split on the stated geometry exactly like the raw
	// .bin above. Substituting DefaultPageSize for a geometry the caller named
	// is wrong on precisely the path where a wrong split can flash and still
	// verify clean (SPEC.md §10.2, §14.12).
	jsonPath := filepath.Join(filepath.Dir(path), "image.json")
	if err := os.WriteFile(jsonPath, []byte(numberArray(page(0, 256))), 0o600); err != nil {
		t.Fatal(err)
	}
	fw, err = LoadFileWithOptions(jsonPath, LoadOptions{PageSize: 128})
	if err != nil {
		t.Fatalf("LoadFileWithOptions(bare array): %v", err)
	}
	if fw.PageSize() != 128 || len(fw.Pages) != 2 {
		t.Errorf("bare array: got %d pages of %d bytes, want 2 of 128", len(fw.Pages), fw.PageSize())
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

// A page size from an image's JSON reaches make() long before newFirmware's
// geometry checks run, so it has to be judged on its own. It used to be checked
// only for divisibility by ChunksPerPage: page_size 1<<40 was a fatal
// "runtime: out of memory" -- fatal, so recover() could not catch it and the
// process simply died -- and MaxInt64-7 (divisible by 8, so it passed) was a
// "makeslice: cap out of range" panic. Both are reachable from a crafted local
// image file and from --fetch, whose 8 MiB message cap bounds the JSON document
// and not the integer inside it.
//
// Every case must come back as an error, promptly. The timing assertion is the
// point: an implementation that allocates first cannot be fast, so a regression
// shows up as a hang or a dead process rather than a quiet pass.
func TestParseJSONPayloadBoundsPageSizeBeforeAllocating(t *testing.T) {
	t.Parallel()
	const ceiling = MaxChunkSize * ChunksPerPage

	tests := []struct {
		name     string
		pageSize string
		wantOK   bool
		// wantPages is checked when wantOK; wantErr is a substring of the
		// message, checked when it is not empty.
		wantPages int
		wantErr   string
	}{
		{name: "unset means the default", pageSize: "0", wantOK: true, wantPages: 1},
		{name: "not divisible by ChunksPerPage", pageSize: "7", wantErr: "not divisible by 8"},
		{name: "negative", pageSize: "-8", wantErr: "not positive"},
		{
			// Divisible by 8 -- MaxInt64 is 8k+7 -- so the old divisibility
			// check waved it through into (len+pageSize-1)/pageSize and
			// make([]byte, pageSize). No substring is asserted because on a
			// 32-bit build the JSON decode rejects it first, for a different
			// and equally acceptable reason.
			name:     "MaxInt64 rounded down to a multiple of ChunksPerPage",
			pageSize: fmt.Sprint(int64(math.MaxInt64) - 7),
		},
		{name: "a terabyte", pageSize: fmt.Sprint(int64(1) << 40), wantErr: "maximum"},
		{name: "the exact ceiling", pageSize: fmt.Sprint(ceiling), wantOK: true, wantPages: 1},
		{name: "one chunk over the ceiling", pageSize: fmt.Sprint(ceiling + ChunksPerPage), wantErr: "maximum"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			body := fmt.Sprintf(`{"app_bin":%s,"page_size":%s,"crc":1}`,
				numberArray(page(0, 16)), tc.pageSize)

			start := time.Now()
			fw, err := ParseJSONPayload([]byte(body))
			elapsed := time.Since(start)

			if elapsed > 2*time.Second {
				t.Errorf("took %s; nothing here should allocate, so this implies a page-sized allocation", elapsed)
			}
			if tc.wantOK {
				if err != nil {
					t.Fatalf("ParseJSONPayload: %v", err)
				}
				if len(fw.Pages) != tc.wantPages {
					t.Errorf("got %d pages, want %d", len(fw.Pages), tc.wantPages)
				}
				return
			}
			if err == nil {
				t.Fatalf("page_size %s was accepted, giving %d pages of %d bytes",
					tc.pageSize, len(fw.Pages), fw.PageSize())
			}
			if !errors.Is(err, ErrBadPageLength) {
				t.Errorf("error %v does not wrap ErrBadPageLength, so the CLI will not classify it", err)
			}
			if tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// The same ceiling applies to a page size the caller states, on every entry
// point that takes LoadOptions: the CLI validates --page-size, but the CLI is
// not the only caller of an exported API.
func TestLoadOptionsPageSizeIsValidated(t *testing.T) {
	t.Parallel()
	const ceiling = MaxChunkSize * ChunksPerPage
	raw := page(0, 16)

	// math.MaxInt-7 is the largest page size an int can hold that is still
	// divisible by ChunksPerPage, so it clears the divisibility rule and has to
	// be caught by the ceiling. Written against math.MaxInt rather than as a
	// terabyte because the tests build for 32-bit too, where a literal 1<<40
	// does not fit an int at all.
	for _, bad := range []int{-8, 7, ceiling + ChunksPerPage, math.MaxInt - 7} {
		if _, err := ParseImage(raw, LoadOptions{PageSize: bad}); err == nil {
			t.Errorf("ParseImage with PageSize %d was accepted", bad)
		} else if !errors.Is(err, ErrBadPageLength) {
			t.Errorf("ParseImage with PageSize %d: %v does not wrap ErrBadPageLength", bad, err)
		}
		// A JSON image ignores the option's value, but an unrepresentable one
		// still says the caller is confused about the geometry, and the flash
		// path is the wrong place to be confused (SPEC.md §10.2).
		body := fmt.Sprintf(`{"app_bin":%s,"crc":1}`, numberArray(raw))
		if _, err := ParseImage([]byte(body), LoadOptions{PageSize: bad}); err == nil {
			t.Errorf("ParseImage(JSON) with PageSize %d was accepted", bad)
		}
	}

	path := filepath.Join(t.TempDir(), "image.bin")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFileWithOptions(path, LoadOptions{PageSize: math.MaxInt - 7}); err == nil {
		t.Error("LoadFileWithOptions with an unrepresentable page size was accepted")
	}
	// The ceiling itself stays usable from every entry point.
	if _, err := LoadFileWithOptions(path, LoadOptions{PageSize: ceiling}); err != nil {
		t.Errorf("LoadFileWithOptions(PageSize %d): %v", ceiling, err)
	}
}

// The version string is untrusted -- a local file or whatever the vendor's
// service returned -- and it is printed to a terminal. It must arrive inert:
// stripped to printable ASCII, the same discipline proto.DecodeString applies to
// device identity strings, and bounded in length.
func TestFirmwareVersionIsSanitised(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"ansi clear screen", "\x1b[2J5.2.0", "[2J5.2.0"},
		{"control bytes", "5.\x00\x072.0\r\n", "5.2.0"},
		{"invalid utf-8", "5.2.0\xff\xfe", "5.2.0"},
		{"kept intact", " 5.2.0 ", "5.2.0"},
		{"at the length limit", strings.Repeat("v", maxVersionLen), strings.Repeat("v", maxVersionLen)},
		{"over the length limit", strings.Repeat("v", maxVersionLen+1), strings.Repeat("v", maxVersionLen) + "..."},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Only the version is marshalled, so that the pages stay a plain
			// number array and the encoding of the payload is not part of what
			// this test exercises.
			version, err := json.Marshal(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			body := fmt.Sprintf(`{"app_bin":%s,"app_version":%s,"crc":1}`,
				numberArray(page(0, 16)), version)
			fw, err := ParseJSONPayload([]byte(body))
			if err != nil {
				t.Fatalf("ParseJSONPayload: %v", err)
			}
			if fw.Version != tc.want {
				t.Errorf("Version = %q, want %q", fw.Version, tc.want)
			}
			for i := 0; i < len(fw.Version); i++ {
				if b := fw.Version[i]; b < 0x20 || b > 0x7E {
					t.Fatalf("Version contains byte 0x%02x at offset %d: an escape sequence reached the terminal", b, i)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The vendor service's page shape
// ---------------------------------------------------------------------------

// chunkedPayload renders the shape the production firmware service actually
// sends, measured 2026-08-21: app_bin as an array of {pg_id, chunks} objects
// where chunks is a MAP from chunk index to that chunk's bytes.
func chunkedPayload(t *testing.T, pages [][]byte, ids []int) []byte {
	t.Helper()
	type pg struct {
		PageID int            `json:"pg_id"`
		Chunks map[string]any `json:"chunks"`
	}
	out := make([]pg, 0, len(pages))
	for i, page := range pages {
		if len(page)%ChunksPerPage != 0 {
			t.Fatalf("test page %d is %d bytes, not divisible by %d", i, len(page), ChunksPerPage)
		}
		n := len(page) / ChunksPerPage
		chunks := make(map[string]any, ChunksPerPage)
		for c := 0; c < ChunksPerPage; c++ {
			vals := make([]int, n)
			for j, b := range page[c*n : (c+1)*n] {
				vals[j] = int(b)
			}
			chunks[strconv.Itoa(c)] = vals
		}
		out = append(out, pg{PageID: ids[i], Chunks: chunks})
	}
	doc := map[string]any{"app_bin": out, "crc": 0x30, "app_version": "APP.05.00.00"}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func seqPage(t *testing.T, start, n int) []byte {
	t.Helper()
	p := make([]byte, n)
	for i := range p {
		p[i] = byte((start + i) % 256)
	}
	return p
}

// The regression test for the defect this shape caused: `firmware flash --fetch`
// could never have worked against the real service. normalisePages decided that
// a first element which is not '[' or '"' meant a flat array of byte values, so
// an object element went to decodeByteArray and failed with "cannot unmarshal
// object into Go value of type json.Number" -- which is exactly what the
// production endpoint returned.
func TestParseVendorChunkedPages(t *testing.T) {
	want := [][]byte{seqPage(t, 0, 320), seqPage(t, 100, 320)}
	fw, err := ParseImage(chunkedPayload(t, want, []int{0, 1}), LoadOptions{})
	if err != nil {
		t.Fatalf("ParseImage: %v", err)
	}
	if len(fw.Pages) != 2 {
		t.Fatalf("pages = %d, want 2", len(fw.Pages))
	}
	for i := range want {
		if !bytes.Equal(fw.Pages[i], want[i]) {
			t.Errorf("page %d does not round-trip", i)
		}
	}
	// 8 chunks of 40 bytes is the real geometry, and it is not the 512-byte
	// default a raw .bin is assumed to use.
	if fw.PageSize() != 320 {
		t.Errorf("PageSize() = %d, want 320", fw.PageSize())
	}
	if !fw.CRCKnown || fw.CRC != 0x30 {
		t.Errorf("crc = %#x known=%v, want 0x30 known", fw.CRC, fw.CRCKnown)
	}
	if fw.Version != "APP.05.00.00" {
		t.Errorf("version = %q", fw.Version)
	}
}

// The chunk map is an object, and JSON object member order carries no meaning.
// Assembling by ranging the decoded map would produce a different image on
// every run -- one that flashes cleanly and boots into nothing.
func TestChunkedPageIgnoresMemberOrder(t *testing.T) {
	page := seqPage(t, 7, 320)
	n := len(page) / ChunksPerPage
	var sb strings.Builder
	sb.WriteString(`{"app_bin":[{"pg_id":0,"chunks":{`)
	// Emit the members in reverse, which is a legal document and the opposite
	// of the order the page must be assembled in.
	for c := ChunksPerPage - 1; c >= 0; c-- {
		if c != ChunksPerPage-1 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `"%d":[`, c)
		for j, b := range page[c*n : (c+1)*n] {
			if j > 0 {
				sb.WriteString(",")
			}
			fmt.Fprintf(&sb, "%d", b)
		}
		sb.WriteString("]")
	}
	sb.WriteString(`}}]}`)

	fw, err := ParseImage([]byte(sb.String()), LoadOptions{})
	if err != nil {
		t.Fatalf("ParseImage: %v", err)
	}
	if !bytes.Equal(fw.Pages[0], page) {
		t.Error("the page was assembled in document order rather than by chunk index")
	}
}

// A page id is a statement about where the page belongs in flash, and array
// position is a different promise. Honouring the id is what stops a reordered
// payload from being written to the wrong addresses -- an image that can flash
// and even verify cleanly, because the device computes the CRC over what it was
// given (SPEC.md 10.2).
func TestChunkedPagesAreOrderedByPageID(t *testing.T) {
	first, second := seqPage(t, 0, 320), seqPage(t, 200, 320)
	// Supplied in reverse: element 0 claims to be page 1.
	payload := chunkedPayload(t, [][]byte{second, first}, []int{1, 0})
	fw, err := ParseImage(payload, LoadOptions{})
	if err != nil {
		t.Fatalf("ParseImage: %v", err)
	}
	if !bytes.Equal(fw.Pages[0], first) || !bytes.Equal(fw.Pages[1], second) {
		t.Error("pages were left in array order instead of pg_id order")
	}
}

// A payload that numbers some of its pages and not others is refused, not
// quietly demoted to array order. Falling back throws away the ids that WERE
// stated, so a page declaring pg_id 5 gets written to address 0 -- the same
// silent mis-assembly a gap or a duplicate is refused for, and just as
// invisible afterwards, since the device computes its CRC over whatever it was
// told to write.
func TestChunkedPagesMustAllStateAPageIDOrNoneAtAll(t *testing.T) {
	page := seqPage(t, 0, 320)
	chunks := func() string {
		n := len(page) / ChunksPerPage
		var sb strings.Builder
		sb.WriteString("{")
		for c := 0; c < ChunksPerPage; c++ {
			if c > 0 {
				sb.WriteString(",")
			}
			fmt.Fprintf(&sb, `"%d":%s`, c, numberArray(page[c*n:(c+1)*n]))
		}
		sb.WriteString("}")
		return sb.String()
	}()

	for _, tc := range []struct {
		name, doc string
	}{
		{
			name: "an object with no pg_id beside one with it",
			doc: fmt.Sprintf(`{"app_bin":[{"pg_id":1,"chunks":%s},{"chunks":%s}],"crc":7}`,
				chunks, chunks),
		},
		{
			name: "a bare page array beside a numbered object",
			doc: fmt.Sprintf(`{"app_bin":[{"pg_id":1,"chunks":%s},%s],"crc":7}`,
				chunks, numberArray(page)),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseImage([]byte(tc.doc), LoadOptions{})
			if err == nil {
				t.Fatal("accepted a payload that states a page id on some elements only")
			}
			if !errors.Is(err, ErrBadPageLength) {
				t.Errorf("error = %v, want it to wrap ErrBadPageLength", err)
			}
			if !strings.Contains(err.Error(), "every page states its id or none does") {
				t.Errorf("error = %v, want it to name the rule", err)
			}
		})
	}

	// A payload that never claimed a page number keeps array order, which is
	// all it can offer.
	t.Run("no element states one", func(t *testing.T) {
		doc := fmt.Sprintf(`{"app_bin":[%s,%s],"crc":7}`, numberArray(page), numberArray(seqPage(t, 9, 320)))
		fw, err := ParseImage([]byte(doc), LoadOptions{})
		if err != nil {
			t.Fatalf("ParseImage: %v", err)
		}
		if !bytes.Equal(fw.Pages[0], page) {
			t.Error("an unnumbered payload must keep array order")
		}
	})
}

func TestChunkedPageRejectsBadGeometry(t *testing.T) {
	page := seqPage(t, 0, 320)
	for _, tc := range []struct {
		name, doc, want string
	}{
		{
			name: "a missing chunk index",
			doc:  `{"app_bin":[{"pg_id":0,"chunks":{"0":[1],"1":[2],"2":[3],"3":[4],"4":[5],"5":[6],"6":[7],"8":[9]}}]}`,
			want: "no chunk",
		},
		{
			name: "too few chunks",
			doc:  `{"app_bin":[{"pg_id":0,"chunks":{"0":[1],"1":[2]}}]}`,
			want: "want exactly",
		},
		{
			name: "no chunks at all",
			doc:  `{"app_bin":[{"pg_id":0,"chunks":{}}]}`,
			want: "no chunks",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseImage([]byte(tc.doc), LoadOptions{})
			if err == nil {
				t.Fatal("accepted a malformed page")
			}
			if !errors.Is(err, ErrBadPageLength) {
				t.Errorf("error = %v, want it to wrap ErrBadPageLength", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}

	t.Run("a duplicate page id", func(t *testing.T) {
		payload := chunkedPayload(t, [][]byte{page, page}, []int{0, 0})
		if _, err := ParseImage(payload, LoadOptions{}); err == nil {
			t.Fatal("accepted two pages claiming the same address")
		} else if !strings.Contains(err.Error(), "more than once") {
			t.Errorf("error = %v", err)
		}
	})

	t.Run("a page id outside the run", func(t *testing.T) {
		payload := chunkedPayload(t, [][]byte{page, page}, []int{0, 7})
		if _, err := ParseImage(payload, LoadOptions{}); err == nil {
			t.Fatal("accepted a gappy page run")
		} else if !strings.Contains(err.Error(), "contiguous") {
			t.Errorf("error = %v", err)
		}
	})
}

// printableASCII keeps its own loop -- it takes a string that may be megabytes
// long and bounds what it returns, where proto.DecodeString takes an
// already-bounded payload and does not -- but it must not keep its own idea of
// what is printable. Every byte, checked against the shared predicate.
func TestPrintableASCIIAppliesTheSharedPredicate(t *testing.T) {
	t.Parallel()
	for i := 0; i < 256; i++ {
		b := byte(i)
		// Sandwiched between two letters: the trailing TrimSpace would
		// otherwise drop a lone space, which the predicate calls printable.
		kept := len(printableASCII(string([]byte{'A', b, 'A'}), 0)) == 3
		if kept != proto.PrintableASCII(b) {
			t.Errorf("printableASCII keeps 0x%02x = %v, but proto.PrintableASCII says %v",
				b, kept, proto.PrintableASCII(b))
		}
	}
}
