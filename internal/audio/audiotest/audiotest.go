// Package audiotest builds small audio files for tests: real headers and
// tags around audio that is silence, or nothing at all, so that no binary
// fixture has to be kept and every test says what it is reading.
package audiotest

import (
	"bytes"
	"encoding/binary"
	"slices"
	"strconv"
)

// Tags are what a built file is tagged with. A zero field is left out.
type Tags struct {
	Title       string
	Artist      string
	AlbumArtist string
	Album       string
	Genre       string
	Composer    string
	Track       int
	Disc        int
	Year        int
	// Picture is a PNG the file carries, the cover of its album.
	Picture []byte
	// Extra are tags the format has no field of its own for, by name, as
	// ReplayGain is written: a TXXX frame of ID3v2, a comment of Vorbis, a
	// freeform atom of MP4.
	Extra map[string]string
}

// extra are the names of the extra tags, in order, so that a file is built
// the same every time.
func (t Tags) extra() []string {
	var names []string
	for name := range t.Extra {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// PNG is the smallest picture there is: one transparent pixel.
var PNG = []byte{
	0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n',
	0, 0, 0, 13, 'I', 'H', 'D', 'R', 0, 0, 0, 1, 0, 0, 0, 1, 8, 6, 0, 0, 0, 0x1f, 0x15, 0xc4, 0x89,
	0, 0, 0, 10, 'I', 'D', 'A', 'T', 0x78, 0x9c, 0x63, 0, 1, 0, 0, 5, 0, 1, 0x0d, 0x0a, 0x2d, 0xb4,
	0, 0, 0, 0, 'I', 'E', 'N', 'D', 0xae, 0x42, 0x60, 0x82,
}

// MP3 builds an MP3 of frames frames, MPEG-1 layer III at 128 kbit/s and
// 44.1 kHz, the first of them a Xing header that counts them when xing is
// set; without it the length is reckoned from the bitrate. Its tag is ID3v2.3.
func MP3(tags Tags, frames int, xing bool) []byte {
	var b bytes.Buffer
	b.Write(id3v2(tags))
	// no padding: 144 * 128000 / 44100
	const length = 417
	header := []byte{0xff, 0xfb, 0x90, 0x00}
	for i := 0; i < frames; i++ {
		frame := make([]byte, length)
		copy(frame, header)
		if i == 0 && xing {
			// after the 32 bytes of side information of a stereo frame
			copy(frame[36:], "Xing")
			binary.BigEndian.PutUint32(frame[40:], 1)
			binary.BigEndian.PutUint32(frame[44:], uint32(frames))
		}
		b.Write(frame)
	}
	return b.Bytes()
}

// MP3Seconds is how long an MP3 of frames frames plays.
func MP3Seconds(frames int) float64 { return float64(frames) * 1152 / 44100 }

func id3v2(tags Tags) []byte {
	var frames bytes.Buffer
	text := func(id, value string) {
		if value == "" {
			return
		}
		frame(&frames, id, append([]byte{0}, value...))
	}
	number := func(id string, n int) {
		if n != 0 {
			text(id, strconv.Itoa(n))
		}
	}
	text("TIT2", tags.Title)
	text("TPE1", tags.Artist)
	text("TPE2", tags.AlbumArtist)
	text("TALB", tags.Album)
	text("TCON", tags.Genre)
	text("TCOM", tags.Composer)
	number("TRCK", tags.Track)
	number("TPOS", tags.Disc)
	number("TYER", tags.Year)
	for _, name := range tags.extra() {
		frame(&frames, "TXXX", append([]byte{0}, name+"\x00"+tags.Extra[name]...))
	}
	if tags.Picture != nil {
		var apic bytes.Buffer
		apic.WriteByte(0)
		apic.WriteString("image/png\x00")
		// the front cover, without a description
		apic.WriteByte(3)
		apic.WriteByte(0)
		apic.Write(tags.Picture)
		frame(&frames, "APIC", apic.Bytes())
	}
	if frames.Len() == 0 {
		return nil
	}
	n := frames.Len()
	header := []byte{'I', 'D', '3', 3, 0, 0,
		byte(n >> 21 & 0x7f), byte(n >> 14 & 0x7f), byte(n >> 7 & 0x7f), byte(n & 0x7f)}
	return append(header, frames.Bytes()...)
}

func frame(b *bytes.Buffer, id string, data []byte) {
	b.WriteString(id)
	_ = binary.Write(b, binary.BigEndian, uint32(len(data)))
	b.Write([]byte{0, 0})
	b.Write(data)
}

// FLAC builds a FLAC stream of seconds seconds at 44.1 kHz, 16 bits, stereo,
// tagged with a Vorbis comment and a PICTURE block. Its frames are left out:
// nothing here decodes them.
func FLAC(tags Tags, seconds int) []byte {
	var b bytes.Buffer
	b.WriteString("fLaC")
	info := make([]byte, 34)
	binary.BigEndian.PutUint16(info[0:], 4096)
	binary.BigEndian.PutUint16(info[2:], 4096)
	packed := uint64(44100)<<44 | uint64(2-1)<<41 | uint64(16-1)<<36 | uint64(44100*seconds)
	binary.BigEndian.PutUint64(info[10:], packed)
	blocks := [][]byte{info, vorbisComment(tags, false)}
	types := []byte{0, 4}
	if tags.Picture != nil {
		var p bytes.Buffer
		for _, v := range []any{uint32(3), uint32(len("image/png"))} {
			_ = binary.Write(&p, binary.BigEndian, v)
		}
		p.WriteString("image/png")
		for _, v := range []uint32{0, 1, 1, 32, 0, uint32(len(tags.Picture))} {
			_ = binary.Write(&p, binary.BigEndian, v)
		}
		p.Write(tags.Picture)
		blocks = append(blocks, p.Bytes())
		types = append(types, 6)
	}
	for i, block := range blocks {
		kind := types[i]
		if i == len(blocks)-1 {
			kind |= 0x80
		}
		n := len(block)
		b.Write([]byte{kind, byte(n >> 16), byte(n >> 8), byte(n)})
		b.Write(block)
	}
	// a frame header and silence, so that the file is a little like music
	b.Write([]byte{0xff, 0xf8, 0x69, 0x18})
	b.Write(make([]byte, 2048))
	return b.Bytes()
}

// vorbisComment is the comment of FLAC and Ogg, the packet prefix of Opus in
// front when opus is set.
func vorbisComment(tags Tags, opus bool) []byte {
	var b bytes.Buffer
	if opus {
		b.WriteString("OpusTags")
	}
	vendor := "go-fs audiotest"
	var comments []string
	add := func(key, value string) {
		if value != "" {
			comments = append(comments, key+"="+value)
		}
	}
	number := func(key string, n int) {
		if n != 0 {
			add(key, strconv.Itoa(n))
		}
	}
	add("TITLE", tags.Title)
	add("ARTIST", tags.Artist)
	add("ALBUMARTIST", tags.AlbumArtist)
	add("ALBUM", tags.Album)
	add("GENRE", tags.Genre)
	add("COMPOSER", tags.Composer)
	number("TRACKNUMBER", tags.Track)
	number("DISCNUMBER", tags.Disc)
	number("DATE", tags.Year)
	for _, name := range tags.extra() {
		add(name, tags.Extra[name])
	}
	_ = binary.Write(&b, binary.LittleEndian, uint32(len(vendor)))
	b.WriteString(vendor)
	_ = binary.Write(&b, binary.LittleEndian, uint32(len(comments)))
	for _, c := range comments {
		_ = binary.Write(&b, binary.LittleEndian, uint32(len(c)))
		b.WriteString(c)
	}
	return b.Bytes()
}

// Opus builds an Ogg Opus stream of seconds seconds, stereo, whose last page
// is at the position that many seconds take at 48 kHz after the pre-skip.
func Opus(tags Tags, seconds int) []byte {
	const preskip = 312
	head := []byte("OpusHead")
	head = append(head, 1, 2, preskip&0xff, preskip>>8)
	head = binary.LittleEndian.AppendUint32(head, 44100)
	head = append(head, 0, 0, 0)
	var b bytes.Buffer
	b.Write(oggPage(0x02, 0, 0, head))
	b.Write(oggPage(0, 0, 1, vorbisComment(tags, true)))
	b.Write(oggPage(0x04, uint64(48000*seconds+preskip), 2, make([]byte, 600)))
	return b.Bytes()
}

// oggPage is one page of the stream 1, carrying one packet.
func oggPage(flags byte, granule uint64, sequence uint32, packet []byte) []byte {
	var segments []byte
	n := len(packet)
	for n >= 255 {
		segments = append(segments, 255)
		n -= 255
	}
	segments = append(segments, byte(n))
	page := []byte{'O', 'g', 'g', 'S', 0, flags}
	page = binary.LittleEndian.AppendUint64(page, granule)
	page = binary.LittleEndian.AppendUint32(page, 1)
	page = binary.LittleEndian.AppendUint32(page, sequence)
	page = append(page, 0, 0, 0, 0, byte(len(segments)))
	page = append(page, segments...)
	page = append(page, packet...)
	binary.LittleEndian.PutUint32(page[22:], oggCRC(page))
	return page
}

// oggCRC is the CRC-32 of Ogg: polynomial 0x04c11db7, not reflected, from 0.
func oggCRC(b []byte) uint32 {
	var crc uint32
	for _, c := range b {
		crc ^= uint32(c) << 24
		for range 8 {
			if crc&0x80000000 != 0 {
				crc = crc<<1 ^ 0x04c11db7
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// M4A builds an MP4 of seconds seconds of AAC at 44.1 kHz, stereo, tagged in
// moov/udta/meta/ilst as iTunes tags it.
func M4A(tags Tags, seconds int) []byte {
	ftyp := atom("ftyp", []byte("M4A \x00\x00\x00\x00M4A isom"))

	mvhd := make([]byte, 100)
	binary.BigEndian.PutUint32(mvhd[12:], 1000)
	binary.BigEndian.PutUint32(mvhd[16:], uint32(1000*seconds))
	hdlr := make([]byte, 25)
	copy(hdlr[8:], "soun")
	entry := make([]byte, 28)
	binary.BigEndian.PutUint16(entry[6:], 1)
	binary.BigEndian.PutUint16(entry[16:], 2)
	binary.BigEndian.PutUint16(entry[18:], 16)
	binary.BigEndian.PutUint32(entry[24:], 44100<<16)
	stsd := append([]byte{0, 0, 0, 0, 0, 0, 0, 1}, atom("mp4a", entry)...)
	trak := atom("trak", atom("mdia", cat(
		atom("hdlr", hdlr),
		atom("minf", atom("stbl", atom("stsd", stsd))))))

	var ilst []byte
	text := func(name, value string) {
		if value != "" {
			ilst = append(ilst, atom(name, data(1, []byte(value)))...)
		}
	}
	text("\xa9nam", tags.Title)
	text("\xa9ART", tags.Artist)
	text("aART", tags.AlbumArtist)
	text("\xa9alb", tags.Album)
	text("\xa9gen", tags.Genre)
	text("\xa9wrt", tags.Composer)
	if tags.Year != 0 {
		text("\xa9day", strconv.Itoa(tags.Year))
	}
	if tags.Track != 0 {
		ilst = append(ilst, atom("trkn", data(0, []byte{0, 0, 0, byte(tags.Track), 0, 0, 0, 0}))...)
	}
	if tags.Disc != 0 {
		ilst = append(ilst, atom("disk", data(0, []byte{0, 0, 0, byte(tags.Disc), 0, 0}))...)
	}
	if tags.Picture != nil {
		ilst = append(ilst, atom("covr", data(14, tags.Picture))...)
	}
	for _, name := range tags.extra() {
		// a freeform atom: who defines the name, the name, and the value
		ilst = append(ilst, atom("----", cat(
			atom("mean", cat([]byte{0, 0, 0, 0}, []byte("com.apple.iTunes"))),
			atom("name", cat([]byte{0, 0, 0, 0}, []byte(name))),
			data(1, []byte(tags.Extra[name]))))...)
	}
	meta := cat([]byte{0, 0, 0, 0}, atom("ilst", ilst))
	moov := atom("moov", cat(atom("mvhd", mvhd), trak, atom("udta", atom("meta", meta))))
	return cat(ftyp, moov, atom("mdat", make([]byte, 2048)))
}

func atom(name string, payload []byte) []byte {
	b := binary.BigEndian.AppendUint32(nil, uint32(8+len(payload)))
	b = append(b, name...)
	return append(b, payload...)
}

// data is the data atom of an ilst entry: its class, a locale of 0, and
// the value.
func data(class byte, value []byte) []byte {
	return atom("data", cat([]byte{0, 0, 0, class, 0, 0, 0, 0}, value))
}

func cat(parts ...[]byte) []byte {
	return bytes.Join(parts, nil)
}

// WAV builds a WAVE file of seconds seconds of 8 kHz, 8 bit mono PCM.
func WAV(seconds int) []byte {
	samples := 8000 * seconds
	var b bytes.Buffer
	b.WriteString("RIFF")
	_ = binary.Write(&b, binary.LittleEndian, uint32(36+samples))
	b.WriteString("WAVEfmt ")
	for _, v := range []any{uint32(16), uint16(1), uint16(1), uint32(8000), uint32(8000), uint16(1), uint16(8)} {
		_ = binary.Write(&b, binary.LittleEndian, v)
	}
	b.WriteString("data")
	_ = binary.Write(&b, binary.LittleEndian, uint32(samples))
	b.Write(bytes.Repeat([]byte{0x80}, samples))
	return b.Bytes()
}
