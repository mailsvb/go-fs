package ftp

import (
	"context"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"go-fs/internal/config"
)

func TestGreetingAndLogin(t *testing.T) {
	server := newServer(t, nil)
	c := connect(t, server)

	c.send("USER john")
	c.expect("232 User logged in")

	c.send("PWD")
	c.expect(`257 "/" is current directory`)

	c.send("QUIT")
	c.expect("221 Goodbye")
}

func TestLoginWithPassword(t *testing.T) {
	server := newServer(t, func(cfg *config.FTP) {
		cfg.Users = []config.User{{Username: "john", Password: "doe"}}
	})

	c := connect(t, server)
	c.send("USER john")
	c.expect("331 Password required for john")
	c.send("PASS doe")
	c.expect("230 Logged on")

	// a wrong password ends the connection
	c = connect(t, server)
	c.send("USER john")
	c.expect("331 Password required for john")
	c.send("PASS wrong")
	c.expect("530 Username or password incorrect")

	// and an unknown user never gets that far
	c = connect(t, server)
	c.send("USER mallory")
	c.expect("530 Not logged in")
}

// The user list is the only source of accounts: a name that is not listed
// cannot log in, and the flags of the entry that matches are applied.
func TestUserListDefinesTheAccounts(t *testing.T) {
	server := newServer(t, func(cfg *config.FTP) {
		cfg.Users = []config.User{{Username: "jane", Password: "secret"}}
	})

	c := connect(t, server)
	c.send("USER stranger")
	c.expect("530 Not logged in")

	c = connect(t, server)
	c.send("USER jane")
	c.expect("331 Password required for jane")
	c.send("PASS secret")
	c.expect("230 Logged on")
}

// Anonymous access is not a feature of its own: it is an account named
// anonymous that logs in without a password, with whatever rights it is given.
func TestAnonymousLogin(t *testing.T) {
	server := newServer(t, func(cfg *config.FTP) { cfg.Users = nil })

	c := connect(t, server)
	c.send("USER anonymous")
	c.expect("530 Not logged in")

	yes := true
	allowed := newServer(t, func(cfg *config.FTP) {
		cfg.Users = []config.User{{
			Username:                  "anonymous",
			AllowLoginWithoutPassword: &yes,
			AllowUserFileRetrieve:     &yes,
		}}
	})
	allowed.write(t, "public.txt", "public")

	c = connect(t, allowed)
	c.send("USER anonymous")
	c.expect("232 User logged in")

	content, reply := c.download(allowed, "RETR public.txt")
	if content != "public" {
		t.Errorf("content = %q", content)
	}
	if !strings.HasPrefix(reply, "226") {
		t.Errorf("reply = %q", reply)
	}
}

func TestFailedPasswordIsDelayed(t *testing.T) {
	server := newServer(t, func(cfg *config.FTP) {
		cfg.Users = []config.User{{Username: "john", Password: "doe"}}
		cfg.LoginFailureDelay = 1
	})

	c := connect(t, server)
	c.send("USER john")
	c.expectCode("331")

	started := time.Now()
	c.send("PASS wrong")
	c.expect("530 Username or password incorrect")
	if elapsed := time.Since(started); elapsed < 900*time.Millisecond {
		t.Errorf("the answer came after %v, it should have been delayed", elapsed)
	}
}

func TestCommandsAreCaseInsensitive(t *testing.T) {
	server := newServer(t, nil)
	c := connect(t, server)

	c.send("user john")
	c.expect("232 User logged in")
	c.send("pWd")
	c.expect(`257 "/" is current directory`)
}

func TestPipelinedCommands(t *testing.T) {
	server := newServer(t, nil)
	c := connect(t, server)

	// three commands in one segment have to be answered separately
	c.raw("USER john\r\nPWD\r\nSYST\r\n")
	c.expect("232 User logged in")
	c.expect(`257 "/" is current directory`)
	c.expect("215 UNIX")
}

func TestCommandSplitAcrossSegments(t *testing.T) {
	server := newServer(t, nil)
	c := connect(t, server)

	c.raw("USER jo")
	time.Sleep(50 * time.Millisecond)
	c.raw("hn\r\n")
	c.expect("232 User logged in")
}

func TestOverlongCommandLineIsRefused(t *testing.T) {
	server := newServer(t, func(cfg *config.FTP) { cfg.MaxCommandLength = 64 })
	c := connect(t, server)

	c.raw("USER " + strings.Repeat("x", 200))
	c.expect("500 Command line too long")
}

func TestUnknownCommand(t *testing.T) {
	server := newServer(t, nil)
	c := connect(t, server)

	// before authentication an unknown command ends the connection
	c.send("BOGUS")
	c.expect("530 Not logged in")

	c = connect(t, server)
	c.login()
	c.send("BOGUS")
	c.expect("500 Command not implemented")
}

func TestPreAuthCommands(t *testing.T) {
	server := newServer(t, nil)

	c := connect(t, server)
	c.send("FEAT")
	feat := c.expectCode("211")
	if !strings.Contains(feat, "AUTH TLS") || !strings.Contains(feat, "211 End") {
		t.Errorf("FEAT = %q", feat)
	}
	// the connection stays usable
	c.send("USER john")
	c.expect("232 User logged in")

	c = connect(t, server)
	c.send("SYST")
	c.expect("215 UNIX")

	c = connect(t, server)
	c.send("HELP")
	c.expectCode("214")

	c = connect(t, server)
	c.send("ACCT x")
	c.expect("202 Account not required")

	c = connect(t, server)
	c.send("QUIT")
	c.expect("221 Goodbye")
}

func TestIdleTimeoutClosesTheConnection(t *testing.T) {
	server := newServer(t, func(cfg *config.FTP) { cfg.IdleTimeout = 1 })
	c := connect(t, server)
	c.expect("421 Timeout, closing control connection")
}

func TestLogoffIsOnlyReportedForALogin(t *testing.T) {
	server := newServer(t, nil)

	c := connect(t, server)
	c.send("QUIT")
	c.expect("221 Goodbye")
	time.Sleep(100 * time.Millisecond)
	if records := server.logs.all("ftp logoff"); len(records) != 0 {
		t.Errorf("a connection that never logged in reported a logoff: %v", records)
	}

	c = connect(t, server)
	c.login()
	c.send("QUIT")
	c.expect("221 Goodbye")
	time.Sleep(150 * time.Millisecond)
	if records := server.logs.all("ftp logoff"); len(records) != 1 {
		t.Errorf("got %d logoff records, want 1", len(records))
	}
	if records := server.logs.all("ftp login"); len(records) != 1 {
		t.Errorf("got %d login records, want 1", len(records))
	}
}

func TestMaxConnections(t *testing.T) {
	server := newServer(t, func(cfg *config.FTP) { cfg.MaxConnections = 2 })

	first := connect(t, server)
	first.login()
	second := connect(t, server)
	second.login()

	// the third connection is accepted and dropped without a greeting
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(server.port)), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 16)
	if n, err := conn.Read(buf); err == nil && n > 0 {
		t.Errorf("the third connection got %q, it should have been refused", buf[:n])
	}
}

func TestShutdownDropsConnections(t *testing.T) {
	server := newServer(t, nil)
	c := connect(t, server)
	c.login()

	if got := server.Connections(); got != 1 {
		t.Fatalf("got %d connections, want 1", got)
	}
	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := server.Connections(); got != 0 {
		t.Errorf("got %d connections after shutdown, want 0", got)
	}
}

func TestNewRejectsMissingFolders(t *testing.T) {
	cfg := config.Default().FTP
	cfg.Basefolder = filepath.Join(t.TempDir(), "nope")
	if _, err := New(cfg, config.FTPS{}, discardLogger()); err == nil {
		t.Error("a missing base folder has to be refused")
	}

	cfg = config.Default().FTP
	cfg.Basefolder = t.TempDir()
	cfg.Users = []config.User{{Username: "john", Basefolder: filepath.Join(t.TempDir(), "nope")}}
	if _, err := New(cfg, config.FTPS{}, discardLogger()); err == nil {
		t.Error("a missing user base folder has to be refused")
	}
}

func TestPerUserBasefolder(t *testing.T) {
	own := t.TempDir()
	server := newServer(t, func(cfg *config.FTP) {
		user := fullUser("john")
		user.Basefolder = own
		cfg.Users = []config.User{user}
	})
	// the file lives in the user's own folder, not the server's
	if err := writeFile(filepath.Join(own, "mine.txt"), "mine"); err != nil {
		t.Fatal(err)
	}
	server.write(t, "theirs.txt", "theirs")

	c := connect(t, server)
	c.login()

	listing, _ := c.download(server, "NLST")
	if !strings.Contains(listing, "mine.txt") || strings.Contains(listing, "theirs.txt") {
		t.Errorf("the user should see their own folder, got %q", listing)
	}
}
