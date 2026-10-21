package service

import (
	"bytes"
	"strings"
	"testing"

	"github.com/jhillyerd/enmime/v2"
)

func TestBuildMIMERoundTrip(t *testing.T) {
	raw, err := BuildMIME(SendOptions{
		FromAddress: "a@x.test",
		FromName:    "Ani Wulandari",
		To:          []string{"b@example.com"},
		Subject:     "Tagihan ✓\r\nBcc: injected@evil.test",
		Text:        "baris satu\nbaris dua",
		HTML:        "<p>halo</p>",
		InReplyTo:   "<prev@x.test>",
		Attachments: []MailAttachment{{Filename: "data.bin", ContentType: "application/octet-stream", Content: bytes.Repeat([]byte{0, 1, 2, 255}, 100)}},
	}, "<id@x.test>")
	if err != nil {
		t.Fatal(err)
	}
	env, err := enmime.ReadEnvelope(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if env.GetHeader("Bcc") != "" {
		t.Fatal("header injection through Subject")
	}
	if env.GetHeader("Subject") != "Tagihan ✓\r\nBcc: injected@evil.test" && env.GetHeader("Subject") != "Tagihan ✓ Bcc: injected@evil.test" {
		t.Fatalf("subject: %q", env.GetHeader("Subject"))
	}
	if strings.ReplaceAll(env.Text, "\r\n", "\n") != "baris satu\nbaris dua" || env.HTML != "<p>halo</p>" || env.GetHeader("In-Reply-To") != "<prev@x.test>" {
		t.Fatalf("body/headers: %q %q", env.Text, env.HTML)
	}
	if len(env.Attachments) != 1 || !bytes.Equal(env.Attachments[0].Content, bytes.Repeat([]byte{0, 1, 2, 255}, 100)) {
		t.Fatal("attachment did not survive")
	}
}

func TestInReplyToCannotAddHeaders(t *testing.T) {
	for in, want := range map[string]string{
		"<a@b.test>":                "<a@b.test>",
		"  <a@b.test> ":             "<a@b.test>",
		"<a@b.test>\r\nX-Marker: 1": "",
		"<a@b.test>\nX-Marker: 1":   "",
		"a@b.test":                  "",
		"<a@b.test> <c@d.test>":     "",
		"":                          "",
	} {
		if got := CleanMsgID(in); got != want {
			t.Errorf("CleanMsgID(%q) = %q, want %q", in, got, want)
		}
	}
	raw, err := BuildMIME(SendOptions{FromAddress: "a@x.test", To: []string{"b@example.com"}, InReplyTo: "<x@y>\r\nX-Marker: 1"}, "<id@x.test>")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "\r\nX-Marker:") {
		t.Fatal("CR/LF in a header value started a new header line")
	}
}

func TestSnippetSkipsQuotedReply(t *testing.T) {
	text := "Daftar:\n\nSiap, sampai ketemu.\n\nOn Wed, Budi <b@x> wrote:\n\n> Rapat?\n>\n> > Kapan?\n"
	if got := CleanSnippet(text, 120); got != "Daftar: Siap, sampai ketemu." {
		t.Fatalf("snippet = %q", got)
	}
}
