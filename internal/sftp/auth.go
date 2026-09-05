package sftp

import (
	"errors"
	"fmt"
	"time"

	"golang.org/x/crypto/ssh"

	"go-fs/internal/config"
	"go-fs/internal/secrets"
	"go-fs/internal/vfs"
)

// account is one resolved entry of sftp.users: its credentials, its rights and
// the folder it sees.
type account struct {
	name     string
	password string
	keys     []ssh.PublicKey
	perms    config.Permissions
	root     *vfs.Root
}

// canPassword reports whether this account may be asked for a password at all.
// An account without one is not open to any password: SSH has no equivalent of
// the FTP anonymous login, so allowLoginWithoutPassword is not honoured here.
func (a *account) canPassword() bool { return a.password != "" }

// buildAccounts resolves the configured users once, at startup, so that a
// malformed key or a missing folder is an error the operator sees immediately
// instead of a login that never succeeds.
func buildAccounts(cfg config.SFTP, serverRoot *vfs.Root) (map[string]*account, error) {
	accounts := make(map[string]*account, len(cfg.Users))
	for i, user := range cfg.Users {
		if _, taken := accounts[user.Username]; taken {
			return nil, fmt.Errorf("sftp.users[%d]: %q is configured twice", i, user.Username)
		}

		resolved := &account{
			name:     user.Username,
			password: user.Password,
			perms:    user.Permissions(),
			root:     serverRoot,
		}
		if user.Basefolder != "" {
			root, err := vfs.New(user.Basefolder)
			if err != nil {
				return nil, fmt.Errorf("sftp.users[%d].basefolder: %w", i, err)
			}
			resolved.root = root
		}
		for k, entry := range user.AuthorizedKeys {
			key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(entry))
			if err != nil {
				return nil, fmt.Errorf("sftp.users[%d].authorizedKeys[%d]: %w", i, k, err)
			}
			resolved.keys = append(resolved.keys, key)
		}
		if !resolved.canPassword() && len(resolved.keys) == 0 {
			return nil, fmt.Errorf("sftp.users[%d]: %q has neither a password nor an authorized key, "+
				"so it could never log in", i, user.Username)
		}
		accounts[user.Username] = resolved
	}
	return accounts, nil
}

func (s *Server) account(name string) *account {
	return s.settings().users[name]
}

var errDenied = errors.New("authentication failed")

// authenticatePassword answers the SSH password method. The reply to a wrong
// password is delayed, as in FTP, so that guessing costs the attacker time.
func (s *Server) authenticatePassword(meta ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
	user := s.account(meta.User())
	if user != nil && user.canPassword() && secrets.Match(string(password), user.password) {
		s.log.Debug("sftp authentication", "user", meta.User(), "method", "password", "success", true)
		return &ssh.Permissions{}, nil
	}
	s.log.Debug("sftp authentication", "user", meta.User(), "method", "password", "success", false)
	if delay := s.settings().cfg.LoginFailureDelay; delay > 0 {
		time.Sleep(time.Duration(delay) * time.Second)
	}
	return nil, errDenied
}

// authenticatePublicKey answers the SSH public key method. The offered key is
// compared against the account's authorized keys; the SSH layer has already
// checked that the client holds the matching private key.
func (s *Server) authenticatePublicKey(meta ssh.ConnMetadata, offered ssh.PublicKey) (*ssh.Permissions, error) {
	user := s.account(meta.User())
	if user != nil {
		wire := offered.Marshal()
		for _, allowed := range user.keys {
			if allowed.Type() == offered.Type() && secrets.MatchBytes(allowed.Marshal(), wire) {
				s.log.Debug("sftp authentication", "user", meta.User(), "method", "publickey", "success", true)
				return &ssh.Permissions{}, nil
			}
		}
	}
	s.log.Debug("sftp authentication", "user", meta.User(), "method", "publickey", "success", false)
	return nil, errDenied
}
