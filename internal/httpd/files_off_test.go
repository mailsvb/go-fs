package httpd

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/mailsvb/go-fs/internal/config"
)

// filesLink is the Files entry of a page's menu.
const filesLink = `<use href="#i-folder"/></svg>Files`

// registryOnlyServer is a server with the files switched off, the registry on
// and open to read, the admin interface, and root, an admin, beside john.
func registryOnlyServer(t *testing.T, tune func(*httpConfig)) *testServer {
	t.Helper()
	folder := t.TempDir()
	return adminServer(t, func(cfg *httpConfig) {
		cfg.PathsWithoutAuth = []string{"^/"}
		cfg.Files = false
		cfg.RegistryBaseFolder = folder
		cfg.RegistryAnonymousRead = true
		cfg.Users = append(cfg.Users, registryUser(pusher, pusherPassword))
		if tune != nil {
			tune(cfg)
		}
	})
}

func TestTheFilesAreOnByDefault(t *testing.T) {
	if !config.Default().HTTP.Files {
		t.Error("http.services.files is off by default")
	}
}

// Off, nothing of the served folder is answered, to anyone and by any way in,
// public paths included.
func TestFilesOffServesNoFile(t *testing.T) {
	server := registryOnlyServer(t, func(cfg *httpConfig) {
		cfg.Users[1].S3 = true
		cfg.WebDAV = true
	})
	server.write(t, "main/a.txt", "x")

	if res, _ := get(t, server, "/main/a.txt"); res.StatusCode != http.StatusNotFound {
		t.Errorf("a public GET was answered %d, want 404", res.StatusCode)
	}
	if res := basic(t, server, http.MethodGet, "/main/a.txt", "john", "doe", nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("an authenticated GET was answered %d, want 404", res.StatusCode)
	}
	if res := basic(t, server, http.MethodPut, "/main/b.txt", "john", "doe", strings.NewReader("y")); res.StatusCode != http.StatusNotFound {
		t.Errorf("a PUT was answered %d, want 404", res.StatusCode)
	}
	if res := basic(t, server, methodPropfind, "/", "john", "doe", nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("a PROPFIND was answered %d, want 404", res.StatusCode)
	}
	if res := basic(t, server, http.MethodGet, "/main/a.txt?go-fs=share", "john", "doe", nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("asking for a share link was answered %d, want 404", res.StatusCode)
	}
	john := login(t, server, "/", "john", "doe")
	if res := withSession(t, server, http.MethodGet, "/main/", john); res.StatusCode != http.StatusNotFound {
		t.Errorf("a session's listing was answered %d, want 404", res.StatusCode)
	}

	client := server.s3Client("john", "doe")
	_, err := client.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(bucket), Key: aws.String("a.txt")})
	if code := errorCode(t, err); code != "AccessDenied" {
		t.Errorf("S3 answered %s, want AccessDenied", code)
	}
}

// A share link handed out while the files were served is not answered once
// they are switched off.
func TestFilesOffAnswersNoShareLink(t *testing.T) {
	server := newServer(t, nil)
	server.write(t, "private/a.txt", "x")
	link := shareLink(t, server, "/private/a.txt")

	set := server.settings()
	cfg := set.cfg
	cfg.Files = false
	if err := server.Reload(cfg, set.https, set.ssh, []config.User{fullUser("john", "doe")}, nil, nil); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if res, _ := get(t, server, link); res.StatusCode != http.StatusNotFound {
		t.Errorf("the share link was answered %d, want 404", res.StatusCode)
	}
}

// What is not the files keeps working: the registry, its page, the admin
// interface, the login.
func TestFilesOffKeepsTheRest(t *testing.T) {
	server := registryOnlyServer(t, nil)

	if res := basic(t, server, http.MethodGet, "/v2/", pusher, pusherPassword, nil); res.StatusCode != http.StatusOK {
		t.Errorf("/v2/ was answered %d, want 200", res.StatusCode)
	}
	res, body := pageRequest(t, server, http.MethodGet, "/?go-fs=registry", nil)
	if res.StatusCode != http.StatusOK {
		t.Errorf("the registry page was answered %d, want 200", res.StatusCode)
	}
	if strings.Contains(body, filesLink) {
		t.Error("the registry page offers Files while they are not served")
	}

	root := login(t, server, "/", "root", "secret")
	res, body = pageRequest(t, server, http.MethodGet, "/?go-fs=admin", root)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("the admin interface was answered %d, want 200", res.StatusCode)
	}
	if strings.Contains(body, filesLink) {
		t.Error("the admin interface offers Files while they are not served")
	}
	if _, body = pageRequest(t, server, http.MethodGet, "/?go-fs=registry", root); strings.Contains(body, filesLink) {
		t.Error("root's registry page offers Files while they are not served")
	}
}

// Off, the Trash page goes with the files even where a trash is set: nothing
// lands in it and nothing can go back. Neither the registry page nor the
// admin interface offers it, and its marker names nothing.
func TestFilesOffHasNoTrash(t *testing.T) {
	server := registryOnlyServer(t, func(cfg *httpConfig) {
		cfg.TrashFolder = t.TempDir()
		cfg.TrashRetentionDays = 30
	})
	root := login(t, server, "/", "root", "secret")
	for _, page := range []string{"/?go-fs=registry", "/?go-fs=admin"} {
		res, body := pageRequest(t, server, http.MethodGet, page, root)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s was answered %d, want 200", page, res.StatusCode)
		}
		if strings.Contains(body, "go-fs=trash") {
			t.Errorf("%s offers the trash while the files are not served", page)
		}
	}
	if res, _ := pageRequest(t, server, http.MethodGet, "/?go-fs=trash", root); res.StatusCode != http.StatusNotFound {
		t.Errorf("the Trash page was answered %d, want 404", res.StatusCode)
	}
}

// Off, / offers what is left to anyone, signed in or not, and never the
// files.
func TestFilesOffTheMainURLOffersWhatIsLeft(t *testing.T) {
	server := registryOnlyServer(t, nil)
	server.write(t, "secret.txt", "x")

	root := login(t, server, "/", "root", "secret")
	for name, session := range map[string]*http.Cookie{"nobody": nil, "root": root} {
		res, body := pageRequest(t, server, http.MethodGet, "/", session)
		if res.StatusCode != http.StatusOK {
			t.Errorf("%s: / was answered %d, want 200", name, res.StatusCode)
			continue
		}
		for _, want := range []string{`href="/?go-fs=registry"`, `href="/?go-fs=admin"`} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: the page has no %s:\n%s", name, want, body)
			}
		}
		if strings.Contains(body, ">Files<") || strings.Contains(body, "secret.txt") {
			t.Errorf("%s: the page offers the files:\n%s", name, body)
		}
	}
	// a program asking for / is not given a page
	if res := basic(t, server, http.MethodGet, "/", "john", "doe", nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("a program's / was answered %d, want 404", res.StatusCode)
	}
}

func TestFilesOffTheMainURLGoesStraightToTheOnlyPlace(t *testing.T) {
	server := newRegistryServer(t, func(cfg *httpConfig) {
		cfg.Files = false
		cfg.PathsWithoutAuth = nil
	})
	res, _ := pageRequest(t, server, http.MethodGet, "/", nil)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/?go-fs=registry" {
		t.Errorf("answer = %d to %q, want 303 to the registry page", res.StatusCode, res.Header.Get("Location"))
	}

	nothing := newServer(t, func(cfg *httpConfig) { cfg.Files = false })
	if res, _ := pageRequest(t, nothing, http.MethodGet, "/", nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("with nothing to offer / was answered %d, want 404", res.StatusCode)
	}
}
