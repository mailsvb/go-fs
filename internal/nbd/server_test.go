//go:build !light && !no_nbd

package nbd

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log/slog"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mailsvb/go-fs/internal/config"
	"github.com/mailsvb/go-fs/internal/disks"
	"github.com/mailsvb/go-fs/internal/sparse"
	"github.com/mailsvb/go-fs/internal/testpki"
)

const mib = 1 << 20

// harness is a running server with a CA, the certificates of its accounts and
// a disk t.raw of 8 MiB.
type harness struct {
	t      *testing.T
	srv    *Server
	ca     *testpki.CA
	roots  *x509.CertPool
	cfg    config.NBD
	users  []config.User
	folder *disks.Folder
	log    *lockedWriter
	certs  map[string]testpki.Pair
}

func yes() *bool { v := true; return &v }

func start(t *testing.T, mutate ...func(*config.NBD)) *harness {
	t.Helper()
	ca := testpki.NewCA(t, "test CA")
	server := ca.Server(t, "127.0.0.1")
	cfg := config.NBD{
		Enabled: true, Address: "127.0.0.1", DisksFolder: t.TempDir(),
		Cert: testpki.Encode(server.CertPEM), Key: testpki.Encode(server.KeyPEM),
		ClientCA: testpki.Encode(ca.CertPEM), MaxConnections: 8, HandshakeTimeout: 5, KeepAlive: 15,
	}
	for _, m := range mutate {
		m(&cfg)
	}
	users := []config.User{
		{Username: "vm", NBD: true, AllowUserFileRetrieve: yes(), AllowUserFileOverwrite: yes()},
		{Username: "viewer", NBD: true, AllowUserFileRetrieve: yes()},
		{Username: "http-only", HTTP: true, AllowUserFileRetrieve: yes(), AllowUserFileOverwrite: yes()},
	}
	logs := &lockedWriter{w: &bytes.Buffer{}}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	srv, err := New(cfg, users, logger)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		_ = srv.Shutdown(context.Background())
		if t.Failed() {
			t.Logf("server log:\n%s", logs.String())
		}
	})
	roots := x509.NewCertPool()
	roots.AddCert(ca.Cert)
	h := &harness{t: t, srv: srv, ca: ca, roots: roots, cfg: cfg, users: users, folder: srv.Folder(), log: logs,
		certs: map[string]testpki.Pair{}}
	if err := h.folder.Create("t.raw", 8*mib); err != nil {
		t.Fatal(err)
	}
	return h
}

type lockedWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.String()
}

func (l *lockedWriter) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(b)
}

// cert is the client certificate of name, the same one every time.
func (h *harness) cert(name string) testpki.Pair {
	if pair, ok := h.certs[name]; ok {
		return pair
	}
	pair := h.ca.Client(h.t, name)
	h.certs[name] = pair
	return pair
}

// login connects and switches to TLS with the certificate of name.
func (h *harness) login(name string) *client {
	h.t.Helper()
	c := dial(h.t, h.srv.Addr().String())
	if err := c.startTLS(h.roots, h.cert(name).TLS(h.t)); err != nil {
		h.t.Fatalf("login as %s: %v", name, err)
	}
	return c
}

// attach logs in as name and attaches disk with structured replies and
// base:allocation, or simple replies when simple says so.
func (h *harness) attach(name, disk string, simple bool) (*client, exportInfo) {
	h.t.Helper()
	c := h.login(name)
	if !simple {
		c.structuredReplies()
		if replies := c.setMeta(disk, contextBaseAllocation); len(replies) != 1 {
			h.t.Fatalf("base:allocation was not selected: %v", replies)
		}
	}
	info, refused := c.goExport(optGo, disk)
	if refused != nil {
		h.t.Fatalf("attaching %s as %s was refused: %#x %s", disk, name, refused.kind, refused.data)
	}
	return c, info
}

func TestOptionsBeforeTLSAreRefused(t *testing.T) {
	h := start(t)
	c := dial(t, h.srv.Addr().String())
	for _, option := range []uint32{optList, optGo, optInfo, optStructuredReply, optSetMetaContext} {
		c.option(option, goRequest("t.raw"))
		r := c.mustReply(option, repErrTLSReqd)
		if len(r.data) == 0 {
			t.Errorf("the refusal of option %d says nothing", option)
		}
	}
	c.option(optExportName, []byte("t.raw"))
	if !c.closed() {
		t.Error("an export asked for before TLS is not hung up on")
	}
}

func TestOnlyAcceptedCertificatesLogIn(t *testing.T) {
	other := testpki.NewCA(t, "other CA")
	cases := map[string]func(h *harness) tls.Certificate{
		"another CA": func(h *harness) tls.Certificate { return other.Client(t, "vm").TLS(t) },
		"expired": func(h *harness) tls.Certificate {
			return h.ca.ClientWith(t, "vm", testpki.Options{NotAfter: time.Now().Add(-time.Minute)}).TLS(t)
		},
		"a server certificate":   func(h *harness) tls.Certificate { return h.ca.Server(t, "vm").TLS(t) },
		"an unknown account":     func(h *harness) tls.Certificate { return h.ca.Client(t, "nobody").TLS(t) },
		"an account without nbd": func(h *harness) tls.Certificate { return h.ca.Client(t, "http-only").TLS(t) },
	}
	for name, cert := range cases {
		t.Run(name, func(t *testing.T) {
			h := start(t)
			c := dial(t, h.srv.Addr().String())
			if err := c.startTLS(h.roots, cert(h)); err == nil {
				t.Fatal("the certificate is accepted")
			}
		})
	}
	t.Run("revoked", func(t *testing.T) {
		var revoked testpki.Pair
		h := start(t, func(cfg *config.NBD) {})
		revoked = h.cert("vm")
		cfg := h.cfg
		cfg.ClientCRL = testpki.Encode(h.ca.CRL(t, revoked.Cert))
		if err := h.srv.Reload(cfg, h.users); err != nil {
			t.Fatal(err)
		}
		c := dial(t, h.srv.Addr().String())
		if err := c.startTLS(h.roots, revoked.TLS(t)); err == nil {
			t.Fatal("a revoked certificate is accepted")
		}
		// the client hears of the refusal before the server logs it
		waitFor(t, func() bool { return strings.Contains(h.log.String(), "is revoked") })
	})
}

func TestReadWriteFlushInBothReplyStyles(t *testing.T) {
	for _, simple := range []bool{true, false} {
		name := "structured"
		if simple {
			name = "simple"
		}
		t.Run(name, func(t *testing.T) {
			h := start(t)
			c, info := h.attach("vm", "t.raw", simple)
			if info.size != 8*mib {
				t.Errorf("the size is %d", info.size)
			}
			if info.flags&tflagReadOnly != 0 || info.flags&tflagSendFlush == 0 || info.flags&tflagSendFUA == 0 ||
				info.flags&tflagSendTrim == 0 || info.flags&tflagWriteZeroes == 0 {
				t.Errorf("the flags are %#x", info.flags)
			}
			if (info.flags&tflagSendDF != 0) == simple {
				t.Errorf("DF is advertised %v with simple replies %v", info.flags&tflagSendDF != 0, simple)
			}
			if info.minimum != 1 || info.preferred != 4096 || info.most != 32*mib {
				t.Errorf("the block sizes are %d/%d/%d", info.minimum, info.preferred, info.most)
			}

			data := bytes.Repeat([]byte("go-fs!"), 10000)
			if r := c.do(cmdWrite, 0, 12345, uint32(len(data)), data); r.errCode != 0 {
				t.Fatalf("write: %d", r.errCode)
			}
			if r := c.do(cmdWrite, cmdFlagFUA, 7*mib, 4, []byte("last")); r.errCode != 0 {
				t.Fatalf("write with FUA: %d", r.errCode)
			}
			if r := c.do(cmdFlush, 0, 0, 0, nil); r.errCode != 0 {
				t.Fatalf("flush: %d", r.errCode)
			}
			r := c.do(cmdRead, 0, 12345, uint32(len(data)), nil)
			if r.errCode != 0 || !bytes.Equal(r.data, data) {
				t.Fatalf("read back %d bytes, error %d", len(r.data), r.errCode)
			}
			if r := c.do(cmdRead, cmdFlagDF, 7*mib, 4, nil); string(r.data) != "last" {
				t.Errorf("read %q", r.data)
			}
			if r := c.do(cmdWrite, 0, 8*mib-2, 4, []byte("over")); r.errCode != errNoSpace {
				t.Errorf("a write past the end = %d, want ENOSPC", r.errCode)
			}
			if r := c.do(cmdRead, 0, 8*mib-2, 4, nil); r.errCode != errInvalid {
				t.Errorf("a read past the end = %d, want EINVAL", r.errCode)
			}
			if !simple {
				if r := c.do(cmdRead, 0, 8*mib-2, 4, nil); len(r.kinds) != 1 || r.kinds[0] != replyTypeError {
					t.Errorf("a failed structured read replies with chunks %v", r.kinds)
				}
			}
			if r := c.do(cmdRead, 0, 0, maxBlock+1, nil); r.errCode != errInvalid {
				t.Errorf("an oversized read = %d, want EINVAL", r.errCode)
			}
			if r := c.do(cmdCache, 0, 0, mib, nil); r.errCode != 0 {
				t.Errorf("cache = %d", r.errCode)
			}
			if r := c.do(99, 0, 0, 0, nil); r.errCode != errInvalid {
				t.Errorf("an unknown command = %d", r.errCode)
			}
			if r := c.do(cmdFlush, 1<<9, 0, 0, nil); r.errCode != errInvalid {
				t.Errorf("an unknown flag = %d", r.errCode)
			}
			c.send(cmdDisc, 0, 1, 0, 0, nil)
			if !c.closed() {
				t.Error("the server does not hang up after NBD_CMD_DISC")
			}
			waitFor(t, func() bool { return strings.Contains(h.log.String(), "nbd detach") })
			if !strings.Contains(h.log.String(), "written=60004") {
				t.Errorf("the detach does not count what was written:\n%s", h.log)
			}
			file, lease, err := h.folder.OpenRead("t.raw")
			if err != nil {
				t.Fatalf("the disk is still held after the client left: %v", err)
			}
			defer lease.Release()
			defer file.Close()
			got := make([]byte, len(data))
			if _, err := file.ReadAt(got, 12345); err != nil || !bytes.Equal(got, data) {
				t.Error("the file does not hold what was written")
			}
		})
	}
}

func TestReadOnlyAccounts(t *testing.T) {
	h := start(t)
	c, info := h.attach("viewer", "t.raw", false)
	if info.flags&tflagReadOnly == 0 || info.flags&tflagMultiConn == 0 || info.flags&tflagSendTrim != 0 {
		t.Errorf("the flags of a read-only export are %#x", info.flags)
	}
	for _, kind := range []uint16{cmdWrite, cmdTrim, cmdWriteZeroes} {
		var data []byte
		if kind == cmdWrite {
			data = []byte("nope")
		}
		if r := c.do(kind, 0, 0, 4, data); r.errCode != errPerm {
			t.Errorf("command %d on a read-only export = %d, want EPERM", kind, r.errCode)
		}
	}
	// a reader does not keep a writer out
	h.attach("vm", "t.raw", false)
	if r := c.do(cmdRead, 0, 0, 4096, nil); r.errCode != 0 {
		t.Errorf("the reader cannot read beside the writer: %d", r.errCode)
	}
}

func TestZeroesTrimAndBlockStatus(t *testing.T) {
	h := start(t)
	c, info := h.attach("vm", "t.raw", false)
	if (info.flags&tflagSendFastZero != 0) != sparse.HolePunching {
		t.Errorf("FAST_ZERO is advertised %v where punching is %v", info.flags&tflagSendFastZero != 0, sparse.HolePunching)
	}
	data := bytes.Repeat([]byte{0xee}, 2*mib)
	if r := c.do(cmdWrite, 0, mib, uint32(len(data)), data); r.errCode != 0 {
		t.Fatal(r.errCode)
	}
	if r := c.do(cmdWriteZeroes, 0, mib, 64*1024, nil); r.errCode != 0 {
		t.Fatalf("write zeroes: %d", r.errCode)
	}
	if r := c.do(cmdWriteZeroes, cmdFlagNoHole|cmdFlagFUA, mib+64*1024, 64*1024, nil); r.errCode != 0 {
		t.Fatalf("write zeroes without a hole: %d", r.errCode)
	}
	if r := c.do(cmdRead, 0, mib, 128*1024, nil); !sparse.IsZero(r.data) {
		t.Error("the zeroed range does not read as zeros")
	}
	if r := c.do(cmdRead, 0, mib+128*1024, 4, nil); !bytes.Equal(r.data, []byte{0xee, 0xee, 0xee, 0xee}) {
		t.Error("write zeroes reached past its range")
	}
	r := c.do(cmdWriteZeroes, cmdFlagFastZero|cmdFlagNoHole, 2*mib, 4096, nil)
	if r.errCode != errNotSup {
		t.Errorf("a fast zero that has to stay allocated = %d, want ENOTSUP", r.errCode)
	}
	if r := c.do(cmdTrim, 0, 2*mib, mib, nil); r.errCode != 0 {
		t.Errorf("trim: %d", r.errCode)
	}
	if r := c.do(cmdTrim, 0, 8*mib, 1, nil); r.errCode != errInvalid {
		t.Errorf("a trim past the end = %d", r.errCode)
	}

	status := c.do(cmdBlockStatus, 0, 0, 8*mib, nil)
	if status.errCode != 0 || len(status.extents) == 0 {
		t.Fatalf("block status = %d with %d extents", status.errCode, len(status.extents))
	}
	var total uint64
	for _, extent := range status.extents {
		total += uint64(extent[0])
		if extent[1]&^(stateHole|stateZero) != 0 {
			t.Errorf("an extent has the state %#x", extent[1])
		}
	}
	if total != 8*mib {
		t.Errorf("the extents cover %d bytes of %d", total, 8*mib)
	}
	if one := c.do(cmdBlockStatus, cmdFlagReqOne, 0, 8*mib, nil); len(one.extents) != 1 {
		t.Errorf("REQ_ONE got %d extents", len(one.extents))
	}
	if r := c.do(cmdBlockStatus, 0, 8*mib-1, 2, nil); r.errCode != errInvalid {
		t.Errorf("block status past the end = %d", r.errCode)
	}
}

// Block status needs base:allocation, and that needs structured replies.
func TestMetaContexts(t *testing.T) {
	h := start(t)
	c := h.login("vm")
	c.option(optSetMetaContext, metaRequest("t.raw", contextBaseAllocation))
	c.mustReply(optSetMetaContext, repErrInvalid)
	c.option(optListMetaContext, metaRequest("t.raw"))
	r := c.mustReply(optListMetaContext, repMetaContext)
	if string(r.data[4:]) != contextBaseAllocation {
		t.Errorf("the listed context is %q", r.data[4:])
	}
	c.mustReply(optListMetaContext, repAck)
	c.structuredReplies()
	if replies := c.setMeta("t.raw", "qemu:dirty-bitmap:x"); len(replies) != 0 {
		t.Errorf("an unknown context was selected: %v", replies)
	}
	// contexts selected for one export do not apply to another
	if replies := c.setMeta("other.raw", contextBaseAllocation); len(replies) != 1 {
		t.Fatal("base:allocation was not selected")
	}
	if _, refused := c.goExport(optGo, "t.raw"); refused != nil {
		t.Fatal(refused)
	}
	if r := c.do(cmdBlockStatus, 0, 0, 4096, nil); r.errCode != errInvalid {
		t.Errorf("block status without a context for the export = %d", r.errCode)
	}
}

func TestInfoListAndUnknownDisks(t *testing.T) {
	h := start(t)
	if err := h.folder.Create("b.raw", mib); err != nil {
		t.Fatal(err)
	}
	c := h.login("viewer")
	c.option(optList, nil)
	var names []string
	for {
		r, err := c.readReply()
		if err != nil {
			t.Fatal(err)
		}
		if r.kind == repAck {
			break
		}
		names = append(names, string(r.data[4:]))
	}
	if strings.Join(names, ",") != "b.raw,t.raw" {
		t.Errorf("NBD_OPT_LIST names %v", names)
	}
	info, refused := c.goExport(optInfo, "b.raw")
	if refused != nil || info.size != mib {
		t.Fatalf("INFO = %+v, %v", info, refused)
	}
	for _, name := range []string{"missing.raw", "../t.raw", ".upload-t.raw", ""} {
		if _, refused := c.goExport(optGo, name); refused == nil || refused.kind != repErrUnknown {
			t.Errorf("GO %q = %v, want ERR_UNKNOWN", name, refused)
		}
	}
	c.option(optExtendedHeaders, nil)
	c.mustReply(optExtendedHeaders, repErrUnsup)
	c.option(optStartTLS, nil)
	c.mustReply(optStartTLS, repErrInvalid)
	// INFO attaches nothing
	if err := h.folder.Resize("b.raw", 2*mib, false); err != nil {
		t.Errorf("INFO left the disk attached: %v", err)
	}
	c.option(optAbort, nil)
	c.mustReply(optAbort, repAck)
	if !c.closed() {
		t.Error("NBD_OPT_ABORT does not end the connection")
	}
}

func TestExportNameAttachesTheOldWay(t *testing.T) {
	h := start(t)
	c := h.login("vm")
	c.option(optExportName, []byte("t.raw"))
	var b [10]byte
	if _, err := io.ReadFull(c.rw, b[:]); err != nil {
		t.Fatal(err)
	}
	if size := uint64(b[0])<<56 | uint64(b[1])<<48 | uint64(b[2])<<40 | uint64(b[3])<<32 |
		uint64(b[4])<<24 | uint64(b[5])<<16 | uint64(b[6])<<8 | uint64(b[7]); size != 8*mib {
		t.Errorf("the size is %d", size)
	}
	if r := c.do(cmdWrite, 0, 0, 2, []byte("ok")); r.errCode != 0 {
		t.Errorf("write: %d", r.errCode)
	}
	other := h.login("vm")
	other.option(optExportName, []byte("missing.raw"))
	if !other.closed() {
		t.Error("an unknown export is not hung up on")
	}
}

func TestOneWriterAtATimeAndTakeover(t *testing.T) {
	h := start(t)
	first, _ := h.attach("vm", "t.raw", false)
	second := dial(t, h.srv.Addr().String())
	if err := second.startTLS(h.roots, h.ca.Client(t, "vm").TLS(t)); err != nil {
		t.Fatal(err)
	}
	_, refused := second.goExport(optGo, "t.raw")
	if refused == nil || refused.kind != repErrPolicy || !strings.Contains(string(refused.data), "read-write by vm") {
		t.Fatalf("a second writer = %v", refused)
	}
	if err := h.folder.Resize("t.raw", 16*mib, false); err == nil {
		t.Error("a disk is resized while attached")
	}

	// the same certificate is the same client, back after losing its
	// connection: it takes over
	again, _ := h.attach("vm", "t.raw", false)
	if !first.closed() {
		t.Error("the connection taken over from is still open")
	}
	if r := again.do(cmdWrite, 0, 0, 3, []byte("new")); r.errCode != 0 {
		t.Errorf("the writer that took over cannot write: %d", r.errCode)
	}
	waitFor(t, func() bool { return strings.Contains(h.log.String(), "same certificate took over") })
}

func TestDisconnectFromTheDisksPage(t *testing.T) {
	h := start(t)
	c, _ := h.attach("vm", "t.raw", false)
	sessions := h.folder.Sessions()
	if len(sessions) != 1 || sessions[0].Account != "vm" || !sessions[0].Writable || sessions[0].Fingerprint == "" {
		t.Fatalf("Sessions = %+v", sessions)
	}
	if !h.folder.Disconnect(sessions[0].ID) {
		t.Fatal("Disconnect found nothing")
	}
	if !c.closed() {
		t.Error("the disconnected client is still connected")
	}
	waitFor(t, func() bool { return len(h.folder.Sessions()) == 0 })
	if err := h.folder.Resize("t.raw", 16*mib, false); err != nil {
		t.Errorf("the disk is still held: %v", err)
	}
}

func TestReloadClosesWhatItNoLongerAdmits(t *testing.T) {
	h := start(t)
	writer, _ := h.attach("vm", "t.raw", false)
	reader, _ := h.attach("viewer", "t.raw", false)

	// the account of the writer may no longer write: it goes, the reader stays
	cfg := h.cfg
	users := append([]config.User(nil), h.users...)
	users[0].AllowUserFileOverwrite = nil
	if err := h.srv.Reload(cfg, users); err != nil {
		t.Fatal(err)
	}
	if !writer.closed() {
		t.Error("a writer that lost the right to write is still connected")
	}
	if r := reader.do(cmdRead, 0, 0, 512, nil); r.errCode != 0 {
		t.Fatalf("the reader was disturbed: %d", r.errCode)
	}

	// revoking the reader's certificate closes it
	cfg.ClientCRL = testpki.Encode(h.ca.CRL(t, h.cert("viewer").Cert))
	if err := h.srv.Reload(cfg, users); err != nil {
		t.Fatal(err)
	}
	if !reader.closed() {
		t.Error("a client whose certificate is revoked is still connected")
	}

	moved := cfg
	moved.Port = 1
	if err := h.srv.Reload(moved, users); err == nil {
		t.Error("a new port is applied without a restart")
	}
}

// fullDisk fails every write the way a full filesystem does.
type fullDisk struct{ image }

func (fullDisk) WriteAt([]byte, int64) (int, error) {
	return 0, &osPathError{err: syscall.ENOSPC}
}

type osPathError struct{ err error }

func (e *osPathError) Error() string { return "write: " + e.err.Error() }
func (e *osPathError) Unwrap() error { return e.err }

func TestAFullDiskIsReportedAsENOSPC(t *testing.T) {
	h := start(t)
	h.srv.wrap = func(img image) image { return fullDisk{img} }
	c, _ := h.attach("vm", "t.raw", false)
	if r := c.do(cmdWrite, 0, 0, 4, []byte("full")); r.errCode != errNoSpace {
		t.Errorf("a write to a full disk = %d, want ENOSPC", r.errCode)
	}
	if !strings.Contains(h.log.String(), "nbd disk full") {
		t.Errorf("the log does not say the disk is full:\n%s", h.log)
	}
}

func TestConnectionLimits(t *testing.T) {
	t.Run("allowed clients", func(t *testing.T) {
		h := start(t, func(cfg *config.NBD) { cfg.AllowedClients = []string{"10.0.0.0/8"} })
		c := &client{t: t}
		raw, err := dialRaw(h.srv.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer raw.Close()
		c.rw = raw
		if !c.closed() {
			t.Error("a client outside nbd.allowedClients is served")
		}
	})
	t.Run("max connections", func(t *testing.T) {
		h := start(t, func(cfg *config.NBD) { cfg.MaxConnections = 1 })
		dial(t, h.srv.Addr().String())
		raw, err := dialRaw(h.srv.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer raw.Close()
		if !(&client{t: t, rw: raw}).closed() {
			t.Error("a connection beyond nbd.maxConnections is served")
		}
	})
	t.Run("handshake timeout", func(t *testing.T) {
		h := start(t, func(cfg *config.NBD) { cfg.HandshakeTimeout = 1 })
		c := dial(t, h.srv.Addr().String())
		started := time.Now()
		if !c.closed() || time.Since(started) > 4*time.Second {
			t.Error("a client that stalls in the handshake is not dropped")
		}
	})
}

// Requests run concurrently and their replies come back whole, each under
// its own cookie.
func TestPipelinedRequests(t *testing.T) {
	h := start(t)
	c, _ := h.attach("vm", "t.raw", false)
	const n = 64
	for i := range n {
		data := bytes.Repeat([]byte{byte(i)}, 64*1024)
		c.send(cmdWrite, 0, uint64(i), uint64(i)*64*1024, uint32(len(data)), data)
	}
	for range n {
		if r, err := c.receive(); err != nil || r.errCode != 0 {
			t.Fatalf("a pipelined write: %v, %d", err, r.errCode)
		}
	}
	for i := range n {
		c.send(cmdRead, 0, uint64(1000+i), uint64(i)*64*1024, 64*1024, nil)
	}
	for range n {
		r, err := c.receive()
		if err != nil {
			t.Fatal(err)
		}
		i := int(r.cookie - 1000)
		if !bytes.Equal(r.data, bytes.Repeat([]byte{byte(i)}, 64*1024)) {
			t.Errorf("the read of block %d came back wrong", i)
		}
	}
}

func TestShutdownSyncsAndLetsGo(t *testing.T) {
	h := start(t)
	c, _ := h.attach("vm", "t.raw", false)
	if r := c.do(cmdWrite, 0, 0, 5, []byte("bye!!")); r.errCode != 0 {
		t.Fatal(r.errCode)
	}
	if err := h.srv.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !c.closed() {
		t.Error("the client is still connected after the shutdown")
	}
	if len(h.folder.Sessions()) != 0 {
		t.Error("the shutdown left a session behind")
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("the condition never held")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
