package disks

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const mib = 1 << 20

func folder(t *testing.T) *Folder {
	t.Helper()
	f, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func create(t *testing.T, f *Folder, name string, size int64) {
	t.Helper()
	if err := f.Create(name, size); err != nil {
		t.Fatal(err)
	}
}

// session is an NBD session whose Close is recorded and releases its lease,
// the way the NBD server's connection does when it ends.
type fakeSession struct {
	*Session
	closed chan struct{}
	lease  *Lease
	file   *os.File
}

func attach(t *testing.T, f *Folder, name string, writable bool, fingerprint string) (*fakeSession, error) {
	t.Helper()
	fake := &fakeSession{closed: make(chan struct{})}
	fake.Session = NewSession(func(string) {
		close(fake.closed)
		go func() {
			if fake.file != nil {
				fake.file.Close()
			}
			fake.lease.Release()
		}()
	})
	fake.Account, fake.Client, fake.Writable, fake.Fingerprint = "vm", "10.0.0.5", writable, fingerprint
	file, lease, err := f.Attach(name, fake.Session)
	if err != nil {
		return nil, err
	}
	fake.file, fake.lease = file, lease
	t.Cleanup(func() { fake.Close("the test is over") })
	return fake, nil
}

func TestValidName(t *testing.T) {
	for _, name := range []string{"web01.raw", "a", "A-b_c.d.img", "0", strings.Repeat("x", MaxNameLength), "con1.raw", "com10"} {
		if err := ValidName(name); err != nil {
			t.Errorf("%q is refused: %v", name, err)
		}
	}
	for _, name := range []string{"", ".hidden", "-x", "_x", "a/b", `a\b`, "a b", "ä.raw", "x.", "..",
		strings.Repeat("x", MaxNameLength+1), "con", "NUL.raw", "com1", "LPT9.img", "aux.tar.gz"} {
		if err := ValidName(name); !errors.Is(err, ErrName) {
			t.Errorf("%q is accepted: %v", name, err)
		}
	}
}

func TestParseSize(t *testing.T) {
	for text, want := range map[string]int64{
		"100G": 100 << 30, "1.5T": 3 << 39, "512MiB": 512 << 20, "4096": 4096, "1000": 1024,
		" 2 gb ": 2 << 30, "1k": 1024, "1": 512, "10M": 10 << 20,
	} {
		got, err := ParseSize(text)
		if err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v, want %d", text, got, err, want)
		}
	}
	for _, text := range []string{"", "0", "-1G", "G", "ten", "2E", "2000P"} {
		if _, err := ParseSize(text); !errors.Is(err, ErrSize) {
			t.Errorf("ParseSize(%q) = %v, want ErrSize", text, err)
		}
	}
}

func TestCreateListResizeRenameDelete(t *testing.T) {
	f := folder(t)
	create(t, f, "web01.raw", 10<<30)
	if err := f.Create("web01.raw", mib); !errors.Is(err, ErrExists) {
		t.Errorf("a second create = %v, want ErrExists", err)
	}
	if err := f.Create("bad name", mib); !errors.Is(err, ErrName) {
		t.Errorf("a bad name = %v", err)
	}
	if err := f.Create("zero.raw", 0); !errors.Is(err, ErrSize) {
		t.Errorf("a zero size = %v", err)
	}
	// work in progress and other files are not disks
	for _, other := range []string{".upload-x.raw", "has space.raw"} {
		if err := os.WriteFile(filepath.Join(f.Base(), other), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(f.Base(), "folder"), 0o700); err != nil {
		t.Fatal(err)
	}
	disks, err := f.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(disks) != 1 || disks[0].Name != "web01.raw" || disks[0].Size != 10<<30 {
		t.Fatalf("List = %+v", disks)
	}

	if err := f.Resize("web01.raw", 5<<30, false); !errors.Is(err, ErrShrink) {
		t.Errorf("shrinking without asking = %v", err)
	}
	if err := f.Resize("web01.raw", 20<<30+1, false); err != nil {
		t.Fatal(err)
	}
	if info, _ := f.Stat("web01.raw"); info.Size() != 20<<30+512 {
		t.Errorf("the resized disk is %d bytes, want it rounded up to a sector", info.Size())
	}
	if err := f.Resize("web01.raw", 5<<30, true); err != nil {
		t.Fatal(err)
	}

	create(t, f, "other.raw", mib)
	if err := f.Rename("web01.raw", "other.raw"); !errors.Is(err, ErrExists) {
		t.Errorf("a rename onto a disk = %v", err)
	}
	if err := f.Rename("web01.raw", "web02.raw"); err != nil {
		t.Fatal(err)
	}
	if f.Exists("web01.raw") || !f.Exists("web02.raw") {
		t.Error("the rename did not move the disk")
	}
	if err := f.Rename("missing.raw", "x.raw"); !errors.Is(err, ErrNotFound) {
		t.Errorf("renaming a missing disk = %v", err)
	}
	if err := f.Delete("web02.raw"); err != nil {
		t.Fatal(err)
	}
	if err := f.Delete("web02.raw"); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting twice = %v", err)
	}
}

// Every pair of modes, as the plan's table has them.
func TestTheLockModes(t *testing.T) {
	f := folder(t)
	create(t, f, "d.raw", mib)
	reg := f.reg
	session := func(fp string) *Session { s := NewSession(nil); s.Fingerprint = fp; return s }
	take := func(mode Mode, s *Session) (*Lease, error) {
		return reg.acquire("d.raw", "d.raw", mode, s, "being tested")
	}
	cases := []struct {
		held, asked Mode
		ok          bool
	}{
		{Reader, Reader, true}, {Reader, Writer, true}, {Reader, Hold, true}, {Reader, Exclusive, false},
		{Writer, Reader, true}, {Writer, Writer, false}, {Writer, Hold, false}, {Writer, Exclusive, false},
		{Hold, Reader, true}, {Hold, Writer, false}, {Hold, Hold, true}, {Hold, Exclusive, false},
		{Exclusive, Reader, false}, {Exclusive, Writer, false}, {Exclusive, Hold, false}, {Exclusive, Exclusive, false},
	}
	names := map[Mode]string{Reader: "reader", Writer: "writer", Hold: "hold", Exclusive: "exclusive"}
	for _, tc := range cases {
		held, err := take(tc.held, session("held"))
		if err != nil {
			t.Fatalf("%s alone: %v", names[tc.held], err)
		}
		asked, err := take(tc.asked, session("asked"))
		if (err == nil) != tc.ok {
			t.Errorf("%s while %s is held: err = %v", names[tc.asked], names[tc.held], err)
		}
		if err != nil && !errors.Is(err, ErrBusy) {
			t.Errorf("a refusal is not ErrBusy: %v", err)
		}
		asked.Release()
		held.Release()
	}
	if len(reg.entries) != 0 {
		t.Errorf("released leases leave %d entries behind", len(reg.entries))
	}
}

func TestAWriterIsAloneUnlessItTakesOverFromItself(t *testing.T) {
	f := folder(t)
	create(t, f, "vm.raw", mib)
	first, err := attach(t, f, "vm.raw", true, "aa")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := attach(t, f, "vm.raw", true, "bb"); !errors.Is(err, ErrBusy) {
		t.Fatalf("a second writer with another certificate = %v", err)
	} else if !strings.Contains(err.Error(), "read-write by vm from 10.0.0.5") {
		t.Errorf("the refusal says %q", err)
	}
	reader, err := attach(t, f, "vm.raw", false, "bb")
	if err != nil {
		t.Fatalf("a reader beside the writer = %v", err)
	}
	if err := f.Resize("vm.raw", 2*mib, false); !errors.Is(err, ErrBusy) {
		t.Errorf("a resize while attached = %v", err)
	}
	if _, _, err := f.OpenRead("vm.raw"); !errors.Is(err, ErrBusy) {
		t.Errorf("a download while attached read-write = %v", err)
	}

	second, err := attach(t, f, "vm.raw", true, "aa")
	if err != nil {
		t.Fatalf("the same certificate does not take over: %v", err)
	}
	select {
	case <-first.closed:
	default:
		t.Error("the session taken over from was not closed")
	}
	sessions := f.Sessions()
	if len(sessions) != 2 || sessions[0] != reader.Session || sessions[1] != second.Session {
		t.Errorf("Sessions = %v", sessions)
	}
	disks, _ := f.List()
	if len(disks[0].Sessions) != 2 {
		t.Errorf("the disk lists %d sessions", len(disks[0].Sessions))
	}

	if !f.Disconnect(second.ID) {
		t.Fatal("Disconnect found nothing")
	}
	<-second.closed
	<-second.released
	if _, _, err := f.OpenRead("vm.raw"); err != nil {
		t.Errorf("a download once the writer is gone = %v", err)
	}
}

// A writer that does not let go when it is closed does not let the one
// taking over write beside it.
func TestATakeoverGivesUpOnASessionThatHangs(t *testing.T) {
	f := folder(t)
	create(t, f, "vm.raw", mib)
	stuck := NewSession(func(string) {})
	stuck.Writable, stuck.Fingerprint = true, "aa"
	_, lease, err := f.Attach("vm.raw", stuck)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	key := f.key("vm.raw")
	done := make(chan error, 1)
	go func() {
		s := NewSession(nil)
		s.Writable, s.Fingerprint = true, "aa"
		_, err := f.reg.acquire(key, "vm.raw", Writer, s, "")
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("the takeover finished at once: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	lease.Release()
	if err := <-done; err != nil {
		t.Errorf("the takeover failed once the old session let go: %v", err)
	}
}

func TestAttachedFilesWriteThrough(t *testing.T) {
	f := folder(t)
	create(t, f, "vm.raw", mib)
	writer, err := attach(t, f, "vm.raw", true, "aa")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.file.WriteAt([]byte("hello"), 4096); err != nil {
		t.Fatal(err)
	}
	reader, err := attach(t, f, "vm.raw", false, "")
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 5)
	if _, err := reader.file.ReadAt(got, 4096); err != nil || string(got) != "hello" {
		t.Errorf("the reader reads %q, %v", got, err)
	}
	if _, err := reader.file.WriteAt([]byte("x"), 0); err == nil {
		t.Error("a read-only session can write")
	}
}

func TestCloneCopiesAndHoldsTheSource(t *testing.T) {
	f := folder(t)
	create(t, f, "base.raw", 8*mib)
	if err := os.WriteFile(filepath.Join(f.Base(), "base.raw"), bytes.Repeat([]byte{7}, 3*mib), 0o640); err != nil {
		t.Fatal(err)
	}
	job, err := f.Clone("base.raw", "copy.raw")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Clone("base.raw", "copy.raw"); !errors.Is(err, ErrBusy) {
		t.Errorf("a second clone into the same name = %v", err)
	}
	waitFor(t, job)
	if state, message, _ := job.State(); state != JobDone {
		t.Fatalf("the clone ended %s: %s", state, message)
	}
	want, _ := os.ReadFile(filepath.Join(f.Base(), "base.raw"))
	got, err := os.ReadFile(filepath.Join(f.Base(), "copy.raw"))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("the clone differs: %v", err)
	}
	if done, total := job.Progress(); done != total || total != 3*mib {
		t.Errorf("progress %d of %d", done, total)
	}
	if _, err := f.Clone("base.raw", "copy.raw"); !errors.Is(err, ErrExists) {
		t.Errorf("a clone onto a disk = %v", err)
	}
	if f.folds {
		if _, err := f.Clone("base.raw", "BASE.raw"); err == nil {
			t.Error("a clone onto itself, spelled otherwise, is accepted")
		}
	}
	if len(f.Jobs()) != 1 || !f.Dismiss(job.ID) || len(f.Jobs()) != 0 {
		t.Error("the finished job cannot be dismissed")
	}

	// a writer keeps a clone off, and a clone keeps a writer off
	writer, err := attach(t, f, "base.raw", true, "aa")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Clone("base.raw", "third.raw"); !errors.Is(err, ErrBusy) {
		t.Errorf("a clone of a disk attached read-write = %v", err)
	}
	writer.Close("done")
	<-writer.released
}

func waitFor(t *testing.T, job *Job) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if state, _, _ := job.State(); state != JobRunning {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the job does not finish")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestUploadInChunks(t *testing.T) {
	f := folder(t)
	data := append(bytes.Repeat([]byte{1}, 3000), make([]byte, 2*mib)...)
	data = append(data, bytes.Repeat([]byte{2}, 100)...)
	total := int64(len(data))
	put := func(start, end int64) (bool, error) {
		return f.WriteChunk("up.raw", Chunk{Start: start, End: end, Total: total}, bytes.NewReader(data[start:end+1]))
	}
	if done, err := put(0, mib-1); err != nil || done {
		t.Fatalf("the first chunk = %v, %v", done, err)
	}
	// a chunk that skips ahead learns where to continue
	var sync *OutOfSyncError
	if _, err := put(mib+10, 2*mib); !errors.As(err, &sync) || sync.Have != mib {
		t.Fatalf("a chunk out of place = %v", err)
	}
	// the same chunk again, its answer lost, is taken as landed
	if done, err := put(0, mib-1); err != nil || done {
		// a start of zero starts over, which is allowed too
		t.Fatalf("the first chunk again = %v, %v", done, err)
	}
	if _, err := put(mib, 2*mib-1); err != nil {
		t.Fatal(err)
	}
	if done, err := put(mib, 2*mib-1); err != nil || done {
		t.Fatalf("a chunk that landed already = %v, %v", done, err)
	}
	// a body shorter than it said is undone
	short := Chunk{Start: 2 * mib, End: total - 1, Total: total}
	if _, err := f.WriteChunk("up.raw", short, bytes.NewReader(data[2*mib:total-10])); !errors.Is(err, ErrShortChunk) {
		t.Fatalf("a short chunk = %v", err)
	}
	if info, _ := os.Stat(f.stagingPath("upload", "up.raw")); info.Size() != 2*mib {
		t.Fatalf("the short chunk left %d bytes staged", info.Size())
	}
	if done, err := put(2*mib, total-1); err != nil || !done {
		t.Fatalf("the last chunk = %v, %v", done, err)
	}
	if f.Exists("up.raw") {
		t.Fatal("the upload is in place before it was finished")
	}
	if disks, _ := f.List(); len(disks) != 0 {
		t.Errorf("an upload in progress is listed: %+v", disks)
	}
	if err := f.FinishUpload("up.raw", total, false); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(f.Base(), "up.raw"))
	if !bytes.Equal(got, data) {
		t.Fatal("the uploaded disk differs from what was sent")
	}

	// a second upload onto the name needs replace, and the disk free
	if _, err := f.WriteChunk("up.raw", Chunk{0, 9, 10}, bytes.NewReader(make([]byte, 10))); err != nil {
		t.Fatal(err)
	}
	if err := f.FinishUpload("up.raw", 10, false); !errors.Is(err, ErrExists) {
		t.Errorf("replacing without asking = %v", err)
	}
	reader, err := attach(t, f, "up.raw", false, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.FinishUpload("up.raw", 10, true); !errors.Is(err, ErrBusy) {
		t.Errorf("replacing an attached disk = %v", err)
	}
	reader.Close("done")
	<-reader.released
	if err := f.FinishUpload("up.raw", 10, true); err != nil {
		t.Fatal(err)
	}
	if info, _ := f.Stat("up.raw"); info.Size() != 10 {
		t.Errorf("the replaced disk is %d bytes", info.Size())
	}
}

func TestSweepRemovesOnlyOldLeftovers(t *testing.T) {
	f := folder(t)
	old := time.Now().Add(-48 * time.Hour)
	for _, name := range []string{".upload-a.raw", ".clone-b.raw", ".upload-fresh.raw", ".other", "c.raw"} {
		path := filepath.Join(f.Base(), name)
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if name != ".upload-fresh.raw" {
			if err := os.Chtimes(path, old, old); err != nil {
				t.Fatal(err)
			}
		}
	}
	f.reg.begin("clone", f.key("b.raw"))
	removed := f.Sweep(24 * time.Hour)
	if len(removed) != 1 || removed[0] != ".upload-a.raw" {
		t.Errorf("Sweep removed %v", removed)
	}
	f.reg.end("clone", f.key("b.raw"))
	if removed := f.Sweep(24 * time.Hour); len(removed) != 1 || removed[0] != ".clone-b.raw" {
		t.Errorf("the second Sweep removed %v", removed)
	}
}

// Two opens of one folder share the locks, which is how the NBD server and
// the Disks page see each other.
func TestOpensOfOneFolderShareTheLocks(t *testing.T) {
	dir := t.TempDir()
	a, _ := Open(dir)
	b, _ := Open(dir + string(filepath.Separator))
	create(t, a, "d.raw", mib)
	if _, err := attach(t, a, "d.raw", true, "aa"); err != nil {
		t.Fatal(err)
	}
	if err := b.Delete("d.raw"); !errors.Is(err, ErrBusy) {
		t.Errorf("the other open deletes an attached disk: %v", err)
	}
}
