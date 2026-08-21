package bootloader

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// DefaultPageSize is the page size assumed when a raw .bin image is loaded.
//
// A raw image carries no page geometry, and the bootloader needs one: pages are
// committed individually and split into exactly ChunksPerPage chunks. 512 bytes
// is the common STM32/SAM flash page and divides cleanly by 8. It is only a
// default — the real geometry of the vendor's images comes from the JSON
// payload, which is why that path ignores this constant entirely.
//
// Do not expect verification to catch a wrong value here. The device computes
// its CRC over what it was told to write, so an image split on the wrong
// geometry — pages landing at the wrong offsets, a tail padded differently —
// can verify perfectly against a matching CRC; and a raw .bin carries no CRC at
// all, so on the very path this default serves there is nothing to compare
// against in the first place (SPEC.md §10.2, §14.12). The guard is the geometry
// validation in newFirmware, which runs before the first WRITE_CHUNK, plus the
// operator knowing the part's real page size (--page-size). And if a badly
// split image is then started — only reachable through UpdateOptions.Force —
// the vendor's "still in bootloader mode, re-flashable" reassurance (SPEC.md
// §10.5) no longer applies: that promise is about a unit which was never told
// to jump.
const DefaultPageSize = 512

// RawImagePad is the byte used to pad the final page of a raw image up to a
// whole page. 0xFF is the erased state of NOR flash, so padding with it leaves
// the tail of the page indistinguishable from never having been written.
const RawImagePad = 0xFF

// Firmware is a firmware image ready to flash: a list of equally sized pages,
// the version string it claims, and the CRC the device is expected to report
// once every page has been committed.
type Firmware struct {
	// Pages are the flash pages in order. Every page has the same length, and
	// that length is divisible by ChunksPerPage.
	Pages [][]byte
	// Version is the image's own version string, if it carried one.
	Version string
	// CRC is the expected verification byte. Meaningful only when CRCKnown.
	//
	// The CRC *algorithm* is unknown (SPEC.md §10.2, §14.12): the device
	// computes it and the host only ever compares the answer against the value
	// supplied alongside the image. Nothing here can compute a CRC from Pages.
	CRC uint8
	// CRCKnown reports whether the image supplied an expected CRC. A raw .bin
	// never does, in which case verification cannot be checked and the caller
	// must warn the user that a bad flash will not be detected.
	CRCKnown bool
}

// PageSize reports the size of each page, or 0 for an empty image.
func (f *Firmware) PageSize() int {
	if f == nil || len(f.Pages) == 0 {
		return 0
	}
	return len(f.Pages[0])
}

// ChunkSize reports the size of each WRITE_CHUNK payload, which is the page
// size divided by ChunksPerPage.
func (f *Firmware) ChunkSize() int { return f.PageSize() / ChunksPerPage }

// TotalBytes reports the size of the image across all pages.
func (f *Firmware) TotalBytes() int { return len(f.Pages) * f.PageSize() }

// maxPageSize is the largest page geometry the wire format can express.
//
// A page is always split into exactly ChunksPerPage WRITE_CHUNK frames, and
// each chunk has to fit one frame whose length lives in a single byte
// (MaxChunkSize, see frames.go). So MaxChunkSize*ChunksPerPage is not a policy
// ceiling but an arithmetic one: a larger page could never be transmitted.
// Derived rather than written out so that a change to the frame layout moves it
// automatically.
const maxPageSize = MaxChunkSize * ChunksPerPage

// validatePageSize checks a page geometry *before* it is used to size an
// allocation.
//
// newFirmware applies the same three rules, but it runs on the finished pages —
// far too late for a page size that arrives from outside. A page_size taken
// verbatim from a JSON image (file- or server-supplied, and only the JSON
// *document* is bounded by MaxMessageBytes, never the integer inside it) used
// to reach make([]byte, pageSize) unchecked: 1<<40 was a fatal
// "runtime: out of memory" that recover cannot catch, and MaxInt64-7 — divisible
// by ChunksPerPage, so the one existing check passed it — a "makeslice: cap out
// of range" panic. A malformed image must be an error, never a dead process
// (SPEC.md §10.3).
//
// The wording of each rule is deliberately identical to the corresponding
// message in newFirmware: the same violation reads the same way whether it is
// caught here or there, and the CLI surfaces either verbatim.
func validatePageSize(pageSize int) error {
	if pageSize <= 0 {
		return fmt.Errorf("%w: page size %d is not positive", ErrBadPageLength, pageSize)
	}
	if pageSize%ChunksPerPage != 0 {
		return fmt.Errorf("%w: data length %d not divisible by %d",
			ErrBadPageLength, pageSize, ChunksPerPage)
	}
	if pageSize > maxPageSize {
		return fmt.Errorf("%w: page of %d bytes yields %d-byte chunks, maximum %d",
			ErrBadPageLength, pageSize, pageSize/ChunksPerPage, MaxChunkSize)
	}
	return nil
}

// LoadOptions tunes how an image file is interpreted.
type LoadOptions struct {
	// PageSize is the page geometry imposed on an image that arrives without
	// one. Zero means DefaultPageSize. Exposed so the CLI can offer
	// --page-size.
	//
	// Only a payload whose pages arrive already split ignores this value; a
	// JSON document that is one string or one flat array of bytes carries no
	// geometry of its own and is split on it exactly like a raw .bin. A
	// non-zero value is also validated on every path: a caller asking for a
	// geometry the wire format cannot express has made a mistake worth naming,
	// and silently ignoring it is how that mistake survives to the next image.
	PageSize int
}

// LoadFile reads a firmware image from disk using the default options.
//
// Both input shapes are accepted:
//
//   - the vendor's JSON payload, {"app_bin"|"app_bin_data"|"firmware": [...],
//     "app_version": "...", "crc": n}, where the pages may be arrays of byte
//     values, base64 strings, or hex strings;
//   - a raw .bin image, which is split into LoadOptions.PageSize pages.
func LoadFile(path string) (*Firmware, error) {
	return LoadFileWithOptions(path, LoadOptions{})
}

// LoadFileWithOptions reads a firmware image from disk with explicit options.
func LoadFileWithOptions(path string, opts LoadOptions) (*Firmware, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("bootloader: reading firmware %s: %w", path, err)
	}
	fw, err := ParseImage(data, opts)
	if err != nil {
		return nil, fmt.Errorf("bootloader: %s: %w", path, err)
	}
	return fw, nil
}

// ParseImage interprets an in-memory firmware image.
//
// The format is detected from the first non-whitespace byte: '{' or '[' means
// the vendor's JSON payload, anything else is treated as a raw binary image.
func ParseImage(data []byte, opts LoadOptions) (*Firmware, error) {
	// An explicit page size is refused here, on every path, rather than only
	// where it is consumed: this is the one place LoadOptions enters the
	// package, so LoadFile, LoadFileWithOptions and ParseImage all inherit the
	// check. Zero is not "invalid", it is "unset" — the zero value of
	// LoadOptions has to keep working — so only a stated geometry is judged.
	if opts.PageSize != 0 {
		if err := validatePageSize(opts.PageSize); err != nil {
			return nil, err
		}
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("%w: file is empty", ErrBadPageLength)
	}
	switch trimmed := bytes.TrimLeft(data, " \t\r\n"); {
	case len(trimmed) == 0:
		return nil, fmt.Errorf("%w: file is empty", ErrBadPageLength)
	case trimmed[0] == '{':
		return ParseJSONPayload(trimmed)
	case trimmed[0] == '[':
		// A bare pages array, with no version or CRC alongside it. The stated
		// geometry is passed through rather than dropped: when the array turns
		// out to be a flat list of byte values it has no page split of its own,
		// so this is the same situation as a raw .bin and the caller's
		// --page-size is the only geometry there is.
		pages, err := normalisePages(trimmed, opts.PageSize)
		if err != nil {
			return nil, err
		}
		return newFirmware(pages, "", 0, false)
	default:
		return parseRawImage(data, opts.PageSize)
	}
}

// parseRawImage splits a flat binary image into equal pages, padding the last
// one out to a whole page with RawImagePad.
func parseRawImage(data []byte, pageSize int) (*Firmware, error) {
	// Zero means "unset"; anything else is validated, including a negative,
	// which used to be silently replaced by the default. Substituting 512 for a
	// stated geometry is wrong on the one path where a wrong split can flash and
	// even verify cleanly (SPEC.md §10.2, §14.12).
	if pageSize == 0 {
		pageSize = DefaultPageSize
	}
	// Every rule has to hold *before* the two expressions below: the capacity
	// arithmetic (len(data)+pageSize-1) overflows on a huge pageSize, and the
	// per-page make() is the unbounded allocation itself.
	if err := validatePageSize(pageSize); err != nil {
		return nil, err
	}
	pages := make([][]byte, 0, (len(data)+pageSize-1)/pageSize)
	for off := 0; off < len(data); off += pageSize {
		end := off + pageSize
		page := make([]byte, pageSize)
		if end > len(data) {
			end = len(data)
			for i := range page {
				page[i] = RawImagePad
			}
		}
		copy(page, data[off:end])
		pages = append(pages, page)
	}
	// A raw image carries no CRC, so verification cannot be checked.
	return newFirmware(pages, "", 0, false)
}

// wirePayload mirrors the JSON the vendor's bootloader WebSocket returns. The
// pages arrive under one of three key names depending on the build that
// produced them (SPEC.md §10.3), so all three are accepted.
type wirePayload struct {
	AppBin     json.RawMessage `json:"app_bin"`
	AppBinData json.RawMessage `json:"app_bin_data"`
	Firmware   json.RawMessage `json:"firmware"`
	AppVersion string          `json:"app_version"`
	Version    string          `json:"version"`
	CRC        json.RawMessage `json:"crc"`
	PageSize   int             `json:"page_size"`
}

// ParseJSONPayload decodes the vendor's firmware JSON.
func ParseJSONPayload(data []byte) (*Firmware, error) {
	var p wirePayload
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("%w: not valid JSON: %w", ErrBadPageLength, err)
	}
	raw := p.AppBin
	if !rawPresent(raw) {
		raw = p.AppBinData
	}
	if !rawPresent(raw) {
		raw = p.Firmware
	}
	if !rawPresent(raw) {
		return nil, fmt.Errorf("%w: none of app_bin, app_bin_data or firmware is present", ErrBadPageLength)
	}
	pages, err := normalisePages(raw, p.PageSize)
	if err != nil {
		return nil, err
	}
	version := p.AppVersion
	if version == "" {
		version = p.Version
	}
	crc, crcKnown, err := decodeCRC(p.CRC)
	if err != nil {
		return nil, err
	}
	return newFirmware(pages, version, crc, crcKnown)
}

// rawPresent reports whether a payload key holds a value worth looking at.
//
// A json.RawMessage is empty only when the key was absent altogether: a key
// written as null is present, and encoding/json stores the four bytes "null"
// verbatim. So a length test alone lets {"app_bin":null,"app_bin_data":[...]}
// — what any serializer that renders unused optional fields rather than
// omitting them produces — stop the three-key fallback (SPEC.md §10.3) at the
// first key and report "firmware payload has no pages" about a payload whose
// pages are right there under the second. decodeCRC has always read null as
// absent; the pages selection now agrees with it.
func rawPresent(m json.RawMessage) bool {
	t := bytes.TrimSpace(m)
	return len(t) > 0 && !bytes.Equal(t, []byte("null"))
}

// decodeCRC accepts the crc field as a JSON number, a decimal string, or a
// 0x-prefixed hex string. An absent or null field means the image has no
// expected CRC.
func decodeCRC(raw json.RawMessage) (uint8, bool, error) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return 0, false, nil
	}
	s = strings.Trim(s, `"`)
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false, nil
	}
	base := 10
	if lower := strings.ToLower(s); strings.HasPrefix(lower, "0x") {
		s, base = lower[2:], 16
	}
	v, err := strconv.ParseUint(s, base, 8)
	if err != nil {
		return 0, false, fmt.Errorf("%w: crc %q is not an 8-bit value: %w", ErrBadPageLength, string(raw), err)
	}
	return uint8(v), true, nil
}

// normalisePages converts the payload's pages field into byte slices.
//
// Three shapes are seen in the wild and all three are handled:
//
//	[[1,2,3,...], [...]]      array of pages, each an array of byte values
//	["AAEC...", "..."]        array of pages, each base64 or hex
//	"AAEC..." / [1,2,3,...]   the whole image in one string or one flat array
//
// In the last case the image has no inherent page split, so pageSize (or
// DefaultPageSize) is applied.
func normalisePages(raw json.RawMessage, pageSize int) ([][]byte, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("%w: firmware payload is empty", ErrBadPageLength)
	}

	// A single string: the whole image, base64 or hex.
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return nil, fmt.Errorf("%w: firmware payload string: %w", ErrBadPageLength, err)
		}
		b, err := decodeByteString(s)
		if err != nil {
			return nil, err
		}
		return splitFlat(b, pageSize)
	}

	var elems []json.RawMessage
	if err := json.Unmarshal(trimmed, &elems); err != nil {
		return nil, fmt.Errorf("%w: firmware payload is neither an array nor a string: %w", ErrBadPageLength, err)
	}
	if len(elems) == 0 {
		return nil, fmt.Errorf("%w: firmware payload has no pages", ErrBadPageLength)
	}

	// A flat array of numbers is the whole image rather than a list of pages.
	// Objects are page elements (see decodePage), so they must not fall into
	// this branch: the vendor service sends exactly that shape, and reading it
	// as a byte list is how `firmware flash --fetch` failed against the real
	// endpoint with "cannot unmarshal object into Go value of type json.Number".
	if first := bytes.TrimSpace(elems[0]); len(first) > 0 && first[0] != '[' && first[0] != '"' && first[0] != '{' {
		b, err := decodeByteArray(trimmed)
		if err != nil {
			return nil, err
		}
		return splitFlat(b, pageSize)
	}

	// The page ceiling applies here, before the second slice is sized and
	// before a single page is decoded — the same before-you-allocate discipline
	// validatePageSize follows, and for the same reason: the element count
	// comes from outside, and only the JSON *document* is bounded by
	// MaxMessageBytes, never the number of things inside it. Three bytes of
	// "[]," per element buy a 24-byte slice header here plus another 24 in the
	// decoded slice, so an 8 MiB message of empty pages used to cost a few
	// hundred megabytes and millions of decodePage calls before newFirmware
	// rejected it for exactly this. It cannot move above the flat-array branch:
	// there the elements are bytes, not pages, and an image larger than
	// MaxPages bytes is perfectly ordinary.
	//
	// The wording is deliberately identical to newFirmware's, so the same
	// violation reads the same way whether it is caught here or there.
	if len(elems) > MaxPages {
		return nil, fmt.Errorf("%w: image has %d pages but the WRITE_CHUNK page id is a 16-bit "+
			"field, so at most %d can be addressed; page %d would wrap onto page 0",
			ErrBadPageLength, len(elems), MaxPages, MaxPages)
	}

	pages := make([][]byte, 0, len(elems))
	for i, e := range elems {
		page, err := decodePage(e)
		if err != nil {
			return nil, fmt.Errorf("%w (page %d)", err, i)
		}
		pages = append(pages, page)
	}
	if err := orderByPageID(elems, pages); err != nil {
		return nil, err
	}
	return pages, nil
}

// orderByPageID reorders pages into pg_id order, when the elements carry one.
//
// The vendor service numbers its pages explicitly, and array position is not
// the same promise as a stated page id: JSON preserves array order, but nothing
// says the server builds the array in flash order, and a page written to the
// wrong address is not a failure that announces itself -- a wrongly assembled
// image can flash and even verify cleanly (SPEC.md §10.2), because the CRC is
// computed by the device over what it was given.
//
// So when ids are present they are the authority, and they must form exactly
// 0..n-1: a gap means a page is missing, a duplicate means one overwrites
// another, and either would otherwise be discovered by a user with a brick.
// Elements without an id are left in array order, which is all a payload that
// never claimed page numbers can offer.
func orderByPageID(elems []json.RawMessage, pages [][]byte) error {
	ids := make([]int, len(elems))
	for i, e := range elems {
		if trimmed := bytes.TrimSpace(e); len(trimmed) == 0 || trimmed[0] != '{' {
			return nil // not the object shape; array order is all there is
		}
		var wp wirePage
		if err := json.Unmarshal(e, &wp); err != nil || wp.PageID == nil {
			return nil // no stated id, so nothing to order by
		}
		ids[i] = *wp.PageID
	}
	seen := make(map[int]bool, len(ids))
	for i, id := range ids {
		if id < 0 || id >= len(ids) {
			return fmt.Errorf("%w: page id %d is outside 0..%d, so the image is not a contiguous "+
				"run of pages (element %d)", ErrBadPageLength, id, len(ids)-1, i)
		}
		if seen[id] {
			return fmt.Errorf("%w: page id %d appears more than once, so one page would overwrite "+
				"another (element %d)", ErrBadPageLength, id, i)
		}
		seen[id] = true
	}
	ordered := make([][]byte, len(pages))
	for i, id := range ids {
		ordered[id] = pages[i]
	}
	copy(pages, ordered)
	return nil
}

// wirePage is the page shape the vendor's firmware service sends: an explicit
// page id and a map from chunk index to that chunk's bytes.
//
// Measured against the production endpoint on 2026-08-21 (SPEC.md §10.3): 165
// pages, ids 0..164, each with chunk keys "0".."7" of 40 bytes, i.e. 320-byte
// pages rather than the 512 a raw .bin is assumed to use.
type wirePage struct {
	PageID *int                       `json:"pg_id"`
	Chunks map[string]json.RawMessage `json:"chunks"`
}

// decodePage converts one page element: an array of byte values, or a base64 or
// hex string.
func decodePage(raw json.RawMessage) ([]byte, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("%w: empty page element", ErrBadPageLength)
	}
	switch trimmed[0] {
	case '[':
		return decodeByteArray(trimmed)
	case '"':
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return nil, fmt.Errorf("%w: page string: %w", ErrBadPageLength, err)
		}
		return decodeByteString(s)
	case '{':
		return decodeChunkedPage(trimmed)
	default:
		return nil, fmt.Errorf("%w: page is neither an array, a string nor a {pg_id, chunks} object", ErrBadPageLength)
	}
}

// decodeChunkedPage assembles one page from the vendor service's chunk map.
//
// The chunks arrive keyed by index as an object, and JSON object member order
// is not meaningful, so the keys are sorted numerically rather than iterated:
// ranging a Go map here would assemble the page in a different order on every
// run, and produce an image that flashes cleanly and boots into nothing.
//
// Exactly ChunksPerPage chunks are required, numbered 0..ChunksPerPage-1. That
// is the same rule the rest of the package already enforces from the other
// direction -- SplitPage divides a page into exactly that many, and Validate
// refuses a page length that is not divisible by it -- expressed on the shape
// the wire happens to use. A short or gappy chunk map would otherwise yield a
// short page, which is precisely the wrongly-split image that can flash and
// verify cleanly (SPEC.md §10.2, §14.12).
func decodeChunkedPage(raw json.RawMessage) ([]byte, error) {
	var wp wirePage
	if err := json.Unmarshal(raw, &wp); err != nil {
		return nil, fmt.Errorf("%w: page object: %w", ErrBadPageLength, err)
	}
	if len(wp.Chunks) == 0 {
		return nil, fmt.Errorf("%w: page object carries no chunks", ErrBadPageLength)
	}
	if len(wp.Chunks) != ChunksPerPage {
		return nil, fmt.Errorf("%w: page has %d chunks, want exactly %d",
			ErrBadPageLength, len(wp.Chunks), ChunksPerPage)
	}
	out := make([]byte, 0, ChunksPerPage*64)
	for i := 0; i < ChunksPerPage; i++ {
		key := strconv.Itoa(i)
		elem, ok := wp.Chunks[key]
		if !ok {
			return nil, fmt.Errorf("%w: page has no chunk %q; chunks must be numbered 0..%d",
				ErrBadPageLength, key, ChunksPerPage-1)
		}
		b, err := decodeChunk(elem)
		if err != nil {
			return nil, fmt.Errorf("%w (chunk %d)", err, i)
		}
		out = append(out, b...)
	}
	return out, nil
}

// decodeChunk converts one chunk: an array of byte values, or a base64/hex
// string, matching what decodePage accepts for a whole page.
func decodeChunk(raw json.RawMessage) ([]byte, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("%w: empty chunk", ErrBadPageLength)
	}
	switch trimmed[0] {
	case '[':
		return decodeByteArray(trimmed)
	case '"':
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return nil, fmt.Errorf("%w: chunk string: %w", ErrBadPageLength, err)
		}
		return decodeByteString(s)
	default:
		return nil, fmt.Errorf("%w: chunk is neither an array nor a string", ErrBadPageLength)
	}
}

// decodeByteArray converts a JSON array of numbers to bytes, rejecting anything
// outside 0-255. json.Number is used so that a value written as 1e2 or 255.0
// does not silently truncate into range.
func decodeByteArray(raw json.RawMessage) ([]byte, error) {
	var nums []json.Number
	if err := json.Unmarshal(raw, &nums); err != nil {
		return nil, fmt.Errorf("%w: byte array: %w", ErrBadPageLength, err)
	}
	out := make([]byte, len(nums))
	for i, n := range nums {
		v, err := strconv.ParseUint(n.String(), 10, 8)
		if err != nil {
			return nil, fmt.Errorf("%w: byte %d (%s) is not in 0-255", ErrBadPageLength, i, n.String())
		}
		out[i] = byte(v)
	}
	return out, nil
}

// decodeByteString converts a page (or whole image) carried as text.
//
// The discrimination is deterministic: a string made entirely of hex digits IS
// hex, full stop. Real firmware base64 is essentially never all-hex-digits
// while hex dumps always are — and because every hex digit is also a valid
// base64 character, any base64 fallback for an all-hex string turns a damaged
// hex dump into something that decodes cleanly as unrelated bytes. An
// even-length gate used to open exactly that trap: a hex image truncated by
// one character (length 4k+3) or with a corrupt digit fell through to base64
// and, on the no-CRC --force path, the garbage got flashed — the failure mode
// the CRC cannot catch and SPEC.md §10.2 warns about. So an odd-length all-hex
// string is now an error naming the likely truncation, never a base64 attempt.
//
// The corollary is accepted deliberately: a genuine base64 string that happens
// to be entirely hex digits with an odd length is rejected too. Vendor
// payloads are number arrays or proper hex dumps (SPEC.md §10.3), and refusing
// an ambiguous image beats flashing garbage. Only strings containing a non-hex
// character try the base64 alphabets, and a failure there reports both
// interpretations. A leading "0x" is honoured as a hex marker only when what
// follows it is hex; otherwise those two characters are payload like any
// others.
func decodeByteString(s string) ([]byte, error) {
	var sb strings.Builder
	sb.Grow(len(s))
	for _, r := range s {
		switch r {
		case ' ', '\t', '\r', '\n':
			// Hex dumps are often whitespace-separated; base64 is often
			// line-wrapped. Neither encoding uses whitespace as data.
		default:
			sb.WriteRune(r)
		}
	}
	clean := sb.String()
	if clean == "" {
		return nil, fmt.Errorf("%w: page string is empty", ErrBadPageLength)
	}
	// A "0x" marker is stripped inside the hex branch, never ahead of the
	// discrimination. '0' and 'x' are both members of every base64 alphabet
	// tried below, so stripping first silently truncated any base64 page whose
	// encoding happens to begin "0x" — roughly one page in 4096 — and returned
	// bytes offset from the real image by a byte and a half. A flat image has
	// no per-page equality constraint to trip over it, so on the no-CRC --force
	// path that is precisely the silent corruption the odd-length gate above
	// exists to prevent.
	hexBody := clean
	if len(clean) >= 2 && clean[0] == '0' && (clean[1] == 'x' || clean[1] == 'X') {
		hexBody = clean[2:]
	}
	if hexBody != "" && firstNonHex(hexBody) < 0 {
		if len(hexBody)%2 != 0 {
			return nil, fmt.Errorf("%w: odd-length hex string (%d digits); the image is likely truncated",
				ErrBadPageLength, len(hexBody))
		}
		b, err := hex.DecodeString(hexBody)
		if err != nil {
			return nil, fmt.Errorf("%w: hex page: %w", ErrBadPageLength, err)
		}
		return b, nil
	}
	// The base64 attempts see the string the caller wrote, prefix included:
	// what looked like a marker was never a marker if the rest is not hex.
	nonHex := firstNonHex(clean)
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	} {
		if b, err := enc.DecodeString(clean); err == nil {
			return b, nil
		}
	}
	// Both interpretations failed; name them both, with the character that
	// ruled hex out, so a corrupted hex dump is diagnosable as one.
	return nil, fmt.Errorf("%w: page string is neither hex (%q at offset %d is not a hex digit) nor base64 in any alphabet",
		ErrBadPageLength, clean[nonHex], nonHex)
}

// firstNonHex reports the offset of the first byte that is not a hex digit, or
// -1 when the whole string is hex.
func firstNonHex(s string) int {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return i
		}
	}
	return -1
}

// splitFlat imposes a page geometry on an image that arrived without one.
//
// The page size here comes straight out of the payload's page_size field, so it
// is untrusted; the defaulting and the validation both live in parseRawImage so
// that this path cannot diverge from the raw-.bin one.
func splitFlat(data []byte, pageSize int) ([][]byte, error) {
	fw, err := parseRawImage(data, pageSize)
	if err != nil {
		return nil, err
	}
	return fw.Pages, nil
}

// newFirmware validates the page geometry and builds the image.
//
// Every page must be the same length, that length must divide by ChunksPerPage,
// the resulting chunk must still fit a single WRITE_CHUNK frame, and there must
// be no more pages than the u16 page id can address.
//
// This is the guard, and it runs before anything is written. Nothing after the
// first WRITE_CHUNK can undo a bad geometry: the device commits page by page,
// and its CRC is computed over what it was told to write, so a wrongly split
// image can verify clean.
func newFirmware(pages [][]byte, version string, crc uint8, crcKnown bool) (*Firmware, error) {
	if len(pages) == 0 {
		return nil, fmt.Errorf("%w: image has no pages", ErrBadPageLength)
	}
	if len(pages) > MaxPages {
		return nil, fmt.Errorf("%w: image has %d pages but the WRITE_CHUNK page id is a 16-bit "+
			"field, so at most %d can be addressed; page %d would wrap onto page 0",
			ErrBadPageLength, len(pages), MaxPages, MaxPages)
	}
	size := len(pages[0])
	if size == 0 {
		return nil, fmt.Errorf("%w: page 0 is empty", ErrBadPageLength)
	}
	for i, p := range pages {
		if len(p) != size {
			return nil, fmt.Errorf("%w: page %d is %d bytes but page 0 is %d; pages must be equal",
				ErrBadPageLength, i, len(p), size)
		}
	}
	if size%ChunksPerPage != 0 {
		return nil, fmt.Errorf("%w: data length %d not divisible by %d",
			ErrBadPageLength, size, ChunksPerPage)
	}
	if size/ChunksPerPage > MaxChunkSize {
		return nil, fmt.Errorf("%w: page of %d bytes yields %d-byte chunks, maximum %d",
			ErrBadPageLength, size, size/ChunksPerPage, MaxChunkSize)
	}
	return &Firmware{
		Pages: pages,
		// The version string is untrusted: it comes from the image's JSON, which
		// is either a local file or whatever the vendor's WebSocket service
		// returned, and it is printed to a terminal and interpolated into CLI
		// output. Sanitising it at the point it enters the package is the
		// durable fix — every consumer of Firmware.Version inherits it, rather
		// than each print site having to remember.
		Version:  printableASCII(version, maxVersionLen),
		CRC:      crc,
		CRCKnown: crcKnown,
	}, nil
}

// maxVersionLen bounds a sanitised version string. Real ones are "5.2.0"; the
// field is free-form JSON and could be megabytes, and a value that long is not
// a version whatever else it is.
const maxVersionLen = 64

// printableASCII reduces an untrusted string to the printable-ASCII discipline
// proto.DecodeString applies to device identity strings, and bounds its length.
//
// Everything outside 0x20-0x7E is dropped, which covers the case that matters:
// an ESC introducing an ANSI control sequence, so that a hostile version string
// or close reason cannot repaint or clear the operator's terminal when it is
// printed. Dropping rather than escaping matches proto.DecodeString, and it is
// also what keeps invalid UTF-8 out.
//
// The rule is deliberately duplicated rather than imported. proto.DecodeString
// takes the []byte of a wire payload already bounded by the one-byte frame
// length, so it needs no ceiling and returns nothing to bound; the strings here
// arrive from JSON and from a close frame, are bounded by nothing useful, and
// converting one to []byte just to reuse eight lines would copy a value that
// may be megabytes long. internal/proto is also the protocol layer: the tail
// that trims and truncates *host-side* text belongs on this side of that line.
func printableASCII(s string, max int) string {
	var sb strings.Builder
	grow := len(s)
	if max > 0 && max < grow {
		grow = max
	}
	sb.Grow(grow)
	truncated := false
	for i := 0; i < len(s); i++ {
		b := s[i]
		if b < 0x20 || b > 0x7E {
			continue
		}
		if max > 0 && sb.Len() >= max {
			truncated = true
			break
		}
		sb.WriteByte(b)
	}
	out := strings.TrimSpace(sb.String())
	if truncated {
		// Say so rather than silently presenting a prefix as the whole value:
		// this is text an operator compares against what they expected.
		out += "..."
	}
	return out
}

// Validate re-checks an externally constructed Firmware. Flash calls it before
// touching the device.
func (f *Firmware) Validate() error {
	if f == nil {
		return errors.New("bootloader: no firmware image")
	}
	_, err := newFirmware(f.Pages, f.Version, f.CRC, f.CRCKnown)
	return err
}
