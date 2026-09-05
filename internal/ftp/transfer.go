package ftp

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
)

// cmdRetr handles RETR.
func cmdRetr(c *conn, arg string) {
	target := c.root.Resolve(c.cwd, arg)
	offset := c.restOffset
	c.restOffset = 0

	if !c.perms.FileRetrieve || !target.Valid {
		c.reply("550", fmt.Sprintf("Transfer failed %q", arg))
		return
	}
	info, err := os.Stat(target.Path)
	if err != nil || !info.Mode().IsRegular() {
		c.reply("550", "File not found")
		return
	}

	c.withData("", func(data net.Conn) (string, string) {
		file, err := os.Open(target.Path)
		if err != nil {
			c.log.Debug("ftp cannot open the file", "error", err)
			return "550", fmt.Sprintf("Transfer failed %q", arg)
		}
		defer func() { _ = file.Close() }()

		if offset > 0 {
			if _, err := file.Seek(offset, io.SeekStart); err != nil {
				return "550", fmt.Sprintf("Transfer failed %q", arg)
			}
		}
		if _, err := io.Copy(data, file); err != nil {
			c.log.Debug("ftp transfer failed", "error", err)
			return "550", fmt.Sprintf("Transfer failed %q", arg)
		}
		c.log.Info("ftp download", "user", c.username, "file", target.Virtual,
			"bytes", info.Size()-offset, "address", c.remoteAddr)
		return "226", fmt.Sprintf("Successfully transferred %q", arg)
	})
}

// cmdStor handles STOR, APPE and STOU.
func cmdStor(kind string) handler {
	return func(c *conn, arg string) {
		appending := kind == "APPE"
		unique := kind == "STOU"

		name := arg
		if unique {
			generated, ok := c.uniqueName(parseListArgument(arg))
			if !ok {
				c.reply("550", "Could not create a unique file name")
				return
			}
			name = generated
		}

		target := c.root.Resolve(c.cwd, name)
		offset := c.restOffset
		c.restOffset = 0
		if appending || unique {
			offset = 0
		}

		if !target.Valid {
			c.reply("550", fmt.Sprintf("Transfer failed %q", name))
			return
		}
		_, statErr := os.Stat(target.Path)
		exists := statErr == nil
		if exists && !c.perms.FileOverwrite {
			c.reply("550", "File already exists")
			return
		}
		if !exists && !c.perms.FileCreate {
			c.reply("550", fmt.Sprintf("Transfer failed %q", name))
			return
		}

		opening := ""
		if unique {
			// RFC 1123 requires the generated name in the opening reply
			opening = "FILE: " + name
		}

		c.withData(opening, func(data net.Conn) (string, string) {
			file, err := c.openForWrite(target.Path, appending, exists, offset)
			if err != nil {
				c.log.Debug("ftp cannot open the file for writing", "error", err)
				return "550", fmt.Sprintf("Transfer failed %q", name)
			}

			written, copyErr := io.Copy(file, data)
			// Report success only once the data has actually reached the file
			// system, and never for a transfer that broke off.
			syncErr := file.Sync()
			closeErr := file.Close()
			if copyErr != nil || syncErr != nil || closeErr != nil {
				c.log.Debug("ftp store failed",
					"copy", copyErr, "sync", syncErr, "close", closeErr)
				return "550", fmt.Sprintf("Transfer failed %q", name)
			}
			c.log.Info("ftp upload", "user", c.username, "file", target.Virtual,
				"bytes", written, "address", c.remoteAddr)
			return "226", fmt.Sprintf("Successfully transferred %q", name)
		})
	}
}

// openForWrite opens the destination with the flags the command needs.
//
// Append flags make the kernel ignore the write position, so a restarted upload
// has to seek into the existing file instead.
func (c *conn) openForWrite(osPath string, appending, exists bool, offset int64) (*os.File, error) {
	if appending {
		return os.OpenFile(osPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	}
	if offset > 0 {
		flags := os.O_WRONLY | os.O_CREATE
		if !exists {
			flags |= os.O_TRUNC
		}
		file, err := os.OpenFile(osPath, flags, 0o644)
		if err != nil {
			return nil, err
		}
		if _, err := file.Seek(offset, io.SeekStart); err != nil {
			_ = file.Close()
			return nil, err
		}
		return file, nil
	}
	return os.OpenFile(osPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
}

// uniqueName picks a name that does not exist yet, for STOU.
func (c *conn) uniqueName(base string) (string, bool) {
	stem := base
	if stem == "" {
		stem = "file"
	}
	for range 100 {
		suffix := make([]byte, 4)
		if _, err := rand.Read(suffix); err != nil {
			return "", false
		}
		candidate := stem + "." + hex.EncodeToString(suffix)
		target := c.root.Resolve(c.cwd, candidate)
		if !target.Valid {
			return "", false
		}
		if _, err := os.Stat(target.Path); err != nil {
			return candidate, true
		}
	}
	return "", false
}

// cmdRest handles REST.
func cmdRest(c *conn, arg string) {
	offset, err := strconv.ParseInt(arg, 10, 64)
	if err != nil || offset < 0 {
		c.restOffset = 0
		c.reply("550", "Wrong restart offset")
		return
	}
	c.restOffset = offset
	c.reply("350", "Restarting at "+strconv.FormatInt(offset, 10))
}

// cmdAbor handles ABOR when no transfer is running. A transfer that is running
// takes the command in the reader, which answers 426 and 226 for it.
func cmdAbor(c *conn, _ string) {
	c.reply("226", "Abort successful")
}
