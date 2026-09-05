package ftp

import (
	"crypto/sha256"
	"crypto/subtle"
	"time"

	"go-fs/internal/config"
	"go-fs/internal/vfs"
)

type loginType int

const (
	loginNone loginType = iota
	loginPassword
	loginNoPassword
)

// secretsMatch compares two secrets without leaking their content through
// timing. Both sides are hashed first so that differing lengths stay
// indistinguishable.
func secretsMatch(a, b string) bool {
	hashA := sha256.Sum256([]byte(a))
	hashB := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(hashA[:], hashB[:]) == 1
}

// validateLoginType decides how the named user may log in. Accounts come from
// the configured user list; a name that is not listed cannot log in. Anonymous
// access is one of those accounts, named "anonymous" with
// allowLoginWithoutPassword set, and gets no special treatment here.
func (c *conn) validateLoginType() loginType {
	cfg := c.server.cfg
	for _, user := range cfg.Users {
		if user.Username != c.username {
			continue
		}
		permissions := user.Permissions()
		if permissions.LoginNoPassword {
			c.applyPermissions(permissions)
			return loginNoPassword
		}
		return loginPassword
	}
	return loginNone
}

// authenticateUser checks the password and applies the account's rights.
func (c *conn) authenticateUser(password string) bool {
	cfg := c.server.cfg
	success := false

	for _, user := range cfg.Users {
		if user.Username != c.username {
			continue
		}
		permissions := user.Permissions()
		if permissions.LoginNoPassword || secretsMatch(password, user.Password) {
			c.applyPermissions(permissions)
			success = true
		}
		break
	}

	c.log.Debug("ftp authentication", "user", c.username, "success", success)
	return success
}

// applyPermissions installs the rights of an account, including its own base
// folder when it has one.
func (c *conn) applyPermissions(permissions config.Permissions) {
	c.perms = permissions
	if permissions.Basefolder == "" {
		c.root = c.server.root
		return
	}
	root, err := vfs.New(permissions.Basefolder)
	if err != nil {
		c.log.Error("ftp cannot use the base folder of the user",
			"user", c.username, "basefolder", permissions.Basefolder, "error", err)
		c.root = c.server.root
		return
	}
	c.root = root
}

// cmdUser handles USER.
func cmdUser(c *conn, arg string) {
	c.username = arg
	switch c.validateLoginType() {
	case loginNone:
		c.reply("530", "Not logged in")
	case loginNoPassword:
		c.authenticated = true
		c.reply("232", "User logged in")
		c.markLoggedIn()
	default:
		c.reply("331", "Password required for "+c.username)
	}
}

// cmdPass handles PASS.
func cmdPass(c *conn, arg string) {
	if c.authenticateUser(arg) {
		c.authenticated = true
		c.reply("230", "Logged on")
		c.markLoggedIn()
		return
	}
	// Answer a wrong password only after a delay, so that guessing passwords
	// costs the attacker time.
	if delay := c.server.cfg.LoginFailureDelay; delay > 0 {
		time.Sleep(time.Duration(delay) * time.Second)
	}
	c.replyAndClose("530", "Username or password incorrect")
}

func (c *conn) markLoggedIn() {
	c.loggedIn = true
	c.log.Info("ftp login", "user", c.username, "address", c.remoteAddr,
		"total", c.server.Connections())
}
