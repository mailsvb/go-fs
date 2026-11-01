package audio

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"time"
)

// The tags say nothing of how long a track is, which a player needs to show
// where in the track it is and to seek. Every container says it somewhere,
// most in a header near the start: FLAC counts its samples in STREAMINFO, MP4
// gives the duration in mvhd, a WAV file the size of its data and the bytes
// per second. An Ogg stream says it at its end, in the position of its last
// page. An MP3 says it in the Xing or VBRI header of its first frame when it
// was encoded with a variable bitrate, and otherwise by its size at the
// bitrate of its frames. An AAC stream without a container is the same, with
// the length of its frames taken on average from the first ones.

// Probe reads the format of the audio in r, which is size bytes long.
func Probe(r io.ReaderAt, size int64) (Format, error) {
	head := make([]byte, 12)
	if n, _ := r.ReadAt(head, 0); n < len(head) {
		return Format{}, ErrNotAudio
	}
	// an ID3v2 tag is put in front of an MP3, and now and then of a FLAC
	start := int64(0)
	if bytes.HasPrefix(head, []byte("ID3")) {
		start = id3v2Size(head)
		if start >= size {
			return Format{}, fmt.Errorf("%w; it may still be arriving", ErrNotAudio)
		}
		if n, _ := r.ReadAt(head, start); n < 4 {
			return Format{}, ErrNotAudio
		}
	}
	switch {
	case bytes.HasPrefix(head, []byte("fLaC")):
		return probeFLAC(r, start, size)
	case start == 0 && bytes.HasPrefix(head, []byte("OggS")):
		return probeOgg(r, size)
	case start == 0 && string(head[4:8]) == "ftyp":
		return probeMP4(r, size)
	case start == 0 && string(head[0:4]) == "RIFF" && string(head[8:12]) == "WAVE":
		return probeWAV(r, size)
	}
	return probeMPEG(r, start, size)
}

// id3v2Size is how many bytes the ID3v2 tag whose header is head takes,
// with its header and its footer.
func id3v2Size(head []byte) int64 {
	n := int64(head[6]&0x7f)<<21 | int64(head[7]&0x7f)<<14 | int64(head[8]&0x7f)<<7 | int64(head[9]&0x7f)
	n += 10
	if head[5]&0x10 != 0 {
		n += 10
	}
	return n
}

// durationOf is how long samples last at rate.
func durationOf(samples int64, rate int) time.Duration {
	if samples <= 0 || rate <= 0 {
		return 0
	}
	return time.Duration(float64(samples) / float64(rate) * float64(time.Second))
}

// bitrateOf is the average bitrate of n bytes that play for d.
func bitrateOf(n int64, d time.Duration) int {
	if n <= 0 || d <= 0 {
		return 0
	}
	return int(float64(n) * 8 / d.Seconds())
}

// probeFLAC reads STREAMINFO, which the format puts first of the metadata
// blocks.
func probeFLAC(r io.ReaderAt, start, size int64) (Format, error) {
	block := make([]byte, 4+34)
	if n, _ := r.ReadAt(block, start+4); n < len(block) || block[0]&0x7f != 0 {
		return Format{}, fmt.Errorf("%w: the FLAC stream starts without STREAMINFO", ErrNotAudio)
	}
	f := Format{Container: "flac", Codec: "flac"}
	readStreamInfo(&f, block[4:])
	f.Bitrate = bitrateOf(size-start, f.Duration)
	return f, nil
}

// readStreamInfo reads the 34 bytes of a FLAC STREAMINFO block into f.
func readStreamInfo(f *Format, info []byte) {
	f.SampleRate = int(info[10])<<12 | int(info[11])<<4 | int(info[12])>>4
	f.Channels = int(info[12]>>1&0x07) + 1
	samples := int64(info[13]&0x0f)<<32 | int64(binary.BigEndian.Uint32(info[14:18]))
	f.Duration = durationOf(samples, f.SampleRate)
}

// probeOgg reads the identification header, which the first page of an Ogg
// stream carries alone, and the position of the last page of the stream.
func probeOgg(r io.ReaderAt, size int64) (Format, error) {
	page := make([]byte, 27+255)
	n, _ := r.ReadAt(page, 0)
	if n < 27 || n < 27+int(page[26]) {
		return Format{}, fmt.Errorf("%w; it may still be arriving", ErrNotAudio)
	}
	serial := binary.LittleEndian.Uint32(page[14:18])
	segments := int(page[26])
	length := 0
	for _, s := range page[27 : 27+segments] {
		length += int(s)
	}
	body := make([]byte, length)
	if n, _ := r.ReadAt(body, int64(27+segments)); n < length {
		return Format{}, fmt.Errorf("%w; it may still be arriving", ErrNotAudio)
	}
	var f Format
	// what the position of a page counts: samples at the rate of the stream,
	// for Opus always at 48 kHz and with the samples it skips at the start
	rate, skip := 0, int64(0)
	switch {
	case bytes.HasPrefix(body, []byte("\x01vorbis")) && len(body) >= 24:
		f = Format{Container: "ogg", Codec: "vorbis", Channels: int(body[11]),
			SampleRate: int(binary.LittleEndian.Uint32(body[12:16]))}
		rate = f.SampleRate
	case bytes.HasPrefix(body, []byte("OpusHead")) && len(body) >= 16:
		f = Format{Container: "opus", Codec: "opus", Channels: int(body[9]), SampleRate: 48000}
		rate, skip = 48000, int64(binary.LittleEndian.Uint16(body[10:12]))
	case bytes.HasPrefix(body, []byte("\x7fFLAC")) && len(body) >= 13+4+34:
		f = Format{Container: "ogg", Codec: "flac"}
		readStreamInfo(&f, body[13+4:])
		rate = f.SampleRate
	default:
		return Format{}, fmt.Errorf("%w: the Ogg stream is neither Vorbis, Opus nor FLAC", ErrNotAudio)
	}
	if granule := lastGranule(r, size, serial); granule > skip {
		f.Duration = durationOf(granule-skip, rate)
	}
	f.Bitrate = bitrateOf(size, f.Duration)
	return f, nil
}

// lastGranule is the position of the last page of the stream serial, read
// from the end of the file, or -1 when there is none.
func lastGranule(r io.ReaderAt, size int64, serial uint32) int64 {
	// a page is at most 65307 bytes, so the last one begins in this tail
	const tail = 65307 + 27
	from := max(size-tail, 0)
	buf := make([]byte, size-from)
	n, _ := r.ReadAt(buf, from)
	buf = buf[:n]
	for i := bytes.LastIndex(buf, []byte("OggS")); i >= 0; i = bytes.LastIndex(buf[:i], []byte("OggS")) {
		if i+27 > len(buf) || buf[i+4] != 0 || binary.LittleEndian.Uint32(buf[i+14:i+18]) != serial {
			continue
		}
		// -1 is a page on which no packet ends
		if granule := int64(binary.LittleEndian.Uint64(buf[i+6 : i+14])); granule >= 0 {
			return granule
		}
	}
	return -1
}

// probeMP4 reads the duration from moov/mvhd, and how the audio is encoded
// from the sample description of the track that is sound.
func probeMP4(r io.ReaderAt, size int64) (Format, error) {
	moov, ok := findBox(r, 0, size, "moov")
	if !ok {
		// the moov box is often written last, when everything else is
		return Format{}, fmt.Errorf("%w: the MP4 file has no moov box; it may still be arriving", ErrNotAudio)
	}
	f := Format{Container: "m4a"}
	if mvhd, ok := findBox(r, moov.start, moov.end, "mvhd"); ok {
		head := make([]byte, 32)
		if n, _ := r.ReadAt(head, mvhd.start); n == len(head) {
			var scale, duration uint64
			if head[0] == 1 {
				scale = uint64(binary.BigEndian.Uint32(head[20:24]))
				duration = binary.BigEndian.Uint64(head[24:32])
			} else {
				scale = uint64(binary.BigEndian.Uint32(head[12:16]))
				duration = uint64(binary.BigEndian.Uint32(head[16:20]))
			}
			if scale > 0 && duration < 1<<62 {
				f.Duration = durationOf(int64(duration), int(scale))
			}
		}
	}
	eachBox(r, moov.start, moov.end, func(b box) bool {
		if b.kind != "trak" || !soundTrack(r, b) {
			return true
		}
		readSampleEntry(r, b, &f)
		return false
	})
	if f.Codec == "" {
		f.Codec = "aac"
	}
	f.Bitrate = bitrateOf(size, f.Duration)
	return f, nil
}

// box is the payload of an MP4 box, from start to end.
type box struct {
	kind       string
	start, end int64
}

// eachBox calls yield with each box between start and end, until it answers
// false or the boxes end.
func eachBox(r io.ReaderAt, start, end int64, yield func(box) bool) {
	head := make([]byte, 16)
	for start+8 <= end {
		if n, _ := r.ReadAt(head[:8], start); n < 8 {
			return
		}
		size := int64(binary.BigEndian.Uint32(head[0:4]))
		header := int64(8)
		switch size {
		case 0:
			size = end - start
		case 1:
			if n, _ := r.ReadAt(head[8:16], start+8); n < 8 {
				return
			}
			size = int64(binary.BigEndian.Uint64(head[8:16]))
			header = 16
		}
		if size < header || start+size > end {
			return
		}
		if !yield(box{kind: string(head[4:8]), start: start + header, end: start + size}) {
			return
		}
		start += size
	}
}

// findBox finds the first box of a kind between start and end.
func findBox(r io.ReaderAt, start, end int64, kind string) (box, bool) {
	var found box
	ok := false
	eachBox(r, start, end, func(b box) bool {
		if b.kind == kind {
			found, ok = b, true
			return false
		}
		return true
	})
	return found, ok
}

// path finds the box a path of kinds leads to from inside parent.
func path(r io.ReaderAt, parent box, kinds ...string) (box, bool) {
	for _, kind := range kinds {
		next, ok := findBox(r, parent.start, parent.end, kind)
		if !ok {
			return box{}, false
		}
		parent = next
	}
	return parent, true
}

// soundTrack reports whether the handler of trak says it is sound.
func soundTrack(r io.ReaderAt, trak box) bool {
	hdlr, ok := path(r, trak, "mdia", "hdlr")
	if !ok {
		return false
	}
	kind := make([]byte, 4)
	n, _ := r.ReadAt(kind, hdlr.start+8)
	return n == 4 && string(kind) == "soun"
}

// readSampleEntry reads the first sample description of a sound track: the
// codec by its four letters, the channels and the sample rate.
func readSampleEntry(r io.ReaderAt, trak box, f *Format) {
	stsd, ok := path(r, trak, "mdia", "minf", "stbl", "stsd")
	if !ok {
		return
	}
	entry := make([]byte, 36)
	if n, _ := r.ReadAt(entry, stsd.start+8); n < len(entry) {
		return
	}
	switch string(entry[4:8]) {
	case "mp4a":
		f.Codec = "aac"
	case "alac":
		f.Codec = "alac"
	case "fLaC":
		f.Codec = "flac"
	case "Opus":
		f.Codec = "opus"
	case ".mp3":
		f.Codec = "mp3"
	}
	f.Channels = int(binary.BigEndian.Uint16(entry[24:26]))
	f.SampleRate = int(binary.BigEndian.Uint32(entry[32:36]) >> 16)
}

// probeWAV reads the fmt and data chunks of a RIFF WAVE file.
func probeWAV(r io.ReaderAt, size int64) (Format, error) {
	f := Format{Container: "wav", Codec: "pcm"}
	byteRate := 0
	head := make([]byte, 8)
	for pos := int64(12); pos+8 <= size; {
		if n, _ := r.ReadAt(head, pos); n < 8 {
			break
		}
		length := int64(binary.LittleEndian.Uint32(head[4:8]))
		switch string(head[0:4]) {
		case "fmt ":
			format := make([]byte, 16)
			if n, _ := r.ReadAt(format, pos+8); n < len(format) {
				return Format{}, fmt.Errorf("%w; it may still be arriving", ErrNotAudio)
			}
			// 1 is PCM, 3 floating point, 0xFFFE says so in an extension
			if code := binary.LittleEndian.Uint16(format[0:2]); code != 1 && code != 3 && code != 0xfffe {
				f.Codec = "wav"
			}
			f.Channels = int(binary.LittleEndian.Uint16(format[2:4]))
			f.SampleRate = int(binary.LittleEndian.Uint32(format[4:8]))
			byteRate = int(binary.LittleEndian.Uint32(format[8:12]))
			f.Bitrate = byteRate * 8
		case "data":
			// a file still being written, or streamed, may say more than
			// there is
			length = min(length, size-pos-8)
			if byteRate > 0 {
				f.Duration = time.Duration(float64(length) / float64(byteRate) * float64(time.Second))
			}
			return f, nil
		}
		pos += 8 + length + length&1
	}
	if f.SampleRate == 0 {
		return Format{}, fmt.Errorf("%w: the WAVE file has no fmt chunk", ErrNotAudio)
	}
	return f, nil
}

// mpegHeader is the header of an MPEG audio frame.
type mpegHeader struct {
	// version is 1, 2, or 25 for 2.5
	version    int
	layer      int
	bitrate    int
	sampleRate int
	mono       bool
	// length is the bytes of the frame, and samples the samples it plays
	length  int
	samples int
}

var mpegBitrates = map[[2]int][16]int{
	{1, 1}: {0, 32, 64, 96, 128, 160, 192, 224, 256, 288, 320, 352, 384, 416, 448, -1},
	{1, 2}: {0, 32, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384, -1},
	{1, 3}: {0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, -1},
	{2, 1}: {0, 32, 48, 56, 64, 80, 96, 112, 128, 144, 160, 176, 192, 224, 256, -1},
	{2, 2}: {0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, -1},
	{2, 3}: {0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, -1},
}

var mpegSampleRates = map[int][3]int{
	1:  {44100, 48000, 32000},
	2:  {22050, 24000, 16000},
	25: {11025, 12000, 8000},
}

// readMPEGHeader reads the frame header at the start of b.
func readMPEGHeader(b []byte) (mpegHeader, bool) {
	if len(b) < 4 || b[0] != 0xff || b[1]&0xe0 != 0xe0 {
		return mpegHeader{}, false
	}
	var h mpegHeader
	switch b[1] >> 3 & 0x03 {
	case 0:
		h.version = 25
	case 2:
		h.version = 2
	case 3:
		h.version = 1
	default:
		return mpegHeader{}, false
	}
	h.layer = 4 - int(b[1]>>1&0x03)
	if h.layer == 4 {
		return mpegHeader{}, false
	}
	table := min(h.version, 2)
	kbps := mpegBitrates[[2]int{table, h.layer}][b[2]>>4]
	rateIndex := int(b[2] >> 2 & 0x03)
	// a free bitrate cannot be measured from the header, and is too rare to
	// be worth measuring otherwise
	if kbps <= 0 || rateIndex == 3 {
		return mpegHeader{}, false
	}
	h.bitrate = kbps * 1000
	h.sampleRate = mpegSampleRates[h.version][rateIndex]
	padding := int(b[2] >> 1 & 0x01)
	h.mono = b[3]>>6 == 3
	switch {
	case h.layer == 1:
		h.samples = 384
		h.length = (12*h.bitrate/h.sampleRate + padding) * 4
	case h.layer == 2 || h.version == 1:
		h.samples = 1152
		h.length = 144*h.bitrate/h.sampleRate + padding
	default:
		h.samples = 576
		h.length = 72*h.bitrate/h.sampleRate + padding
	}
	return h, true
}

// adtsHeader is the header of an AAC frame without a container.
type adtsHeader struct {
	sampleRate int
	channels   int
	length     int
}

var adtsSampleRates = [...]int{96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050, 16000, 12000, 11025, 8000, 7350}

func readADTSHeader(b []byte) (adtsHeader, bool) {
	if len(b) < 7 || b[0] != 0xff || b[1]&0xf6 != 0xf0 {
		return adtsHeader{}, false
	}
	index := int(b[2] >> 2 & 0x0f)
	length := int(b[3]&0x03)<<11 | int(b[4])<<3 | int(b[5])>>5
	if index >= len(adtsSampleRates) || length < 7 {
		return adtsHeader{}, false
	}
	return adtsHeader{sampleRate: adtsSampleRates[index], channels: int(b[2]&0x01)<<2 | int(b[3]>>6),
		length: length}, true
}

// probeMPEG finds the first frame of an MP3 or an AAC stream after start,
// within the junk an encoder may leave in front of it: one whose next frame
// follows where its length says, which a stray 0xFF in the junk does not.
func probeMPEG(r io.ReaderAt, start, size int64) (Format, error) {
	buf := make([]byte, 64<<10)
	n, _ := r.ReadAt(buf, start)
	buf = buf[:n]
	follows := func(at int64, read func([]byte) bool) bool {
		if at >= size {
			// the only frame there is
			return true
		}
		next := make([]byte, 7)
		got, _ := r.ReadAt(next, at)
		return read(next[:got])
	}
	isMPEG := func(b []byte) bool { _, ok := readMPEGHeader(b); return ok }
	isADTS := func(b []byte) bool { _, ok := readADTSHeader(b); return ok }
	for i := 0; i+4 <= len(buf); i++ {
		if buf[i] != 0xff {
			continue
		}
		at := start + int64(i)
		if h, ok := readMPEGHeader(buf[i:]); ok && follows(at+int64(h.length), isMPEG) {
			return mp3Format(r, buf[i:], at, size, h), nil
		}
		if h, ok := readADTSHeader(buf[i:]); ok && follows(at+int64(h.length), isADTS) {
			return adtsFormat(r, at, size, h), nil
		}
	}
	return Format{}, ErrNotAudio
}

// mp3Format reads the length of an MP3 from the Xing, Info or VBRI header of
// its first frame, or from its size at the bitrate of that frame.
func mp3Format(r io.ReaderAt, frame []byte, at, size int64, h mpegHeader) Format {
	f := Format{Container: "mp3", Codec: "mp3", SampleRate: h.sampleRate, Channels: 2}
	if h.mono {
		f.Channels = 1
	}
	audio := size - at
	tag := make([]byte, 3)
	if n, _ := r.ReadAt(tag, size-128); n == 3 && string(tag) == "TAG" {
		audio -= 128
	}
	// the Xing header comes after the side information, whose length
	// depends on the version and the channels
	side := 32
	switch {
	case h.version == 1 && h.mono:
		side = 17
	case h.version != 1 && !h.mono:
		side = 17
	case h.version != 1:
		side = 9
	}
	frames := int64(0)
	if x := 4 + side; len(frame) >= x+12 {
		if magic := string(frame[x : x+4]); (magic == "Xing" || magic == "Info") && frame[x+7]&0x01 != 0 {
			frames = int64(binary.BigEndian.Uint32(frame[x+8 : x+12]))
		}
	}
	if v := 4 + 32; frames == 0 && len(frame) >= v+18 && string(frame[v:v+4]) == "VBRI" {
		frames = int64(binary.BigEndian.Uint32(frame[v+14 : v+18]))
	}
	if frames > 0 {
		f.Duration = durationOf(frames*int64(h.samples), h.sampleRate)
		f.Bitrate = bitrateOf(audio, f.Duration)
		return f
	}
	f.Bitrate = h.bitrate
	f.Duration = time.Duration(float64(audio) * 8 / float64(h.bitrate) * float64(time.Second))
	return f
}

// adtsFormat reckons the length of an AAC stream from the average length of
// its first frames, each of which plays 1024 samples.
func adtsFormat(r io.ReaderAt, at, size int64, h adtsHeader) Format {
	f := Format{Container: "aac", Codec: "aac", SampleRate: h.sampleRate, Channels: h.channels}
	head := make([]byte, 7)
	pos, frames := at, int64(0)
	for frames < 256 && pos < size {
		if n, _ := r.ReadAt(head, pos); n < len(head) {
			break
		}
		next, ok := readADTSHeader(head)
		if !ok {
			break
		}
		pos += int64(next.length)
		frames++
	}
	if frames > 0 && pos > at {
		average := float64(pos-at) / float64(frames)
		total := float64(size-at) / average
		f.Duration = time.Duration(total * 1024 / float64(h.sampleRate) * float64(time.Second))
		f.Bitrate = bitrateOf(size-at, f.Duration)
	}
	return f
}
