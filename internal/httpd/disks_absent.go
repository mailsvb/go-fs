//go:build light || no_nbd

package httpd

import "net/http"

// diskFolder is nothing in a binary built without the Disks page.
type diskFolder = *struct{}

func openDisks(string) (diskFolder, error) { return nil, nil }

func disksPageAction(*http.Request) string { return "" }

func (s *Server) handleDisks(*settings, http.ResponseWriter, *http.Request, string) {}

func offersDisks(*settings, credential) bool { return false }

func disksURL() string { return "" }

func (s *Server) sweepDisks() {}
