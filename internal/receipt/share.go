package receipt

import (
	"bytes"
	"fmt"
	"image"
	_ "image/gif" // the pictures a phone shares
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/makiuchi-d/gozxing"
	"github.com/makiuchi-d/gozxing/qrcode"

	"babki.my/babki/internal/family"
)

// What one share takes: a few files — photos of receipts, a statement, letters.
const (
	maxShare      = 40 << 20
	maxShareFiles = 10
	maxShareFile  = 20 << 20
	// maxPixels is the longest side a photo is read at: a receipt's QR code
	// reads from a scaled phone photo, and in a second rather than ten.
	maxPixels = 2000
)

// handleShare takes what the installed app was handed by «Поделиться» on a
// phone (the manifest's share_target): photos of receipts, whose QR code is
// read here; a statement of «Проверка чеков»; letters (.eml). Nothing is kept
// on the device. A single receipt left waiting for its row opens the quick
// add with it; anything else lands on «Деньги» with what it brought.
func (h *Handler) handleShare(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	r.Body = http.MaxBytesReader(w, r.Body, maxShare)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		http.Redirect(w, r, "/money?shared=bad", http.StatusSeeOther)
		return
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()
	var found []Receipt
	single := 0
	files := r.MultipartForm.File["files"]
	if len(files) > maxShareFiles {
		files = files[:maxShareFiles]
	}
	for _, fh := range files {
		f, err := fh.Open()
		if err != nil {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(f, maxShareFile))
		_ = f.Close()
		if err != nil {
			continue
		}
		switch sharedKind(fh.Filename, fh.Header.Get("Content-Type"), data) {
		case "json":
			if rs, err := ParseFNS(data); err == nil {
				found = append(found, rs...)
			}
		case "mail":
			if rs, err := ParseMail(data); err == nil {
				found = append(found, rs...)
			}
		case "image":
			if text, ok := ReadQR(data); ok {
				if rc, ok := QRText(text); ok {
					found = append(found, rc)
					single++
				}
			}
		}
	}
	// A QR line shared as text: copied from a scanner app.
	for _, field := range []string{"text", "url", "title"} {
		if rc, ok := QRText(r.FormValue(field)); ok {
			found = append(found, rc)
			single++
			break
		}
	}
	if len(found) == 0 {
		http.Redirect(w, r, "/money?shared=none", http.StatusSeeOther)
		return
	}
	res, err := h.svc.Import(r.Context(), p.SpaceID, found)
	if err != nil {
		http.Redirect(w, r, "/money?shared=bad", http.StatusSeeOther)
		return
	}
	if len(found) == 1 && single == 1 && len(res.WaitingIDs) == 1 {
		http.Redirect(w, r, "/?add&receipt="+res.WaitingIDs[0].String(), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/money?shared=%d.%d.%d.%d.%d.%d",
		res.Found, res.Attached, res.Waiting, res.Enriched, res.Known, res.Split), http.StatusSeeOther)
}

// sharedKind tells a shared file by its name, its type or its first bytes.
func sharedKind(name, contentType string, data []byte) string {
	mediaType, _, _ := mime.ParseMediaType(contentType)
	ext := strings.ToLower(path.Ext(name))
	head := bytes.TrimSpace(data[:min(len(data), 64)])
	switch {
	case strings.HasPrefix(mediaType, "image/"):
		return "image"
	case mediaType == "application/json" || ext == ".json" || len(head) > 0 && (head[0] == '[' || head[0] == '{'):
		return "json"
	case mediaType == "message/rfc822" || ext == ".eml":
		return "mail"
	}
	if _, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
		return "image"
	}
	return ""
}

// QRText is the receipt of a cash receipt's QR line (t=…&s=…&fn=…&i=…&fp=…&n=…).
func QRText(text string) (Receipt, bool) {
	q, err := url.ParseQuery(strings.TrimSpace(text))
	if err != nil {
		return Receipt{}, false
	}
	params := map[string]string{}
	for _, k := range []string{"t", "s", "fn", "i", "fp", "n"} {
		params[k] = q.Get(k)
	}
	return qrReceipt(params, SourceQR)
}

// ReadQR is the text of the QR code a photo shows, when it shows one.
func ReadQR(data []byte) (string, bool) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return "", false
	}
	img = shrink(img, maxPixels)
	bmp, err := gozxing.NewBinaryBitmapFromImage(img)
	if err != nil {
		return "", false
	}
	hints := map[gozxing.DecodeHintType]any{gozxing.DecodeHintType_TRY_HARDER: true}
	res, err := qrcode.NewQRCodeReader().Decode(bmp, hints)
	if err != nil {
		return "", false
	}
	return res.GetText(), true
}

// shrink scales a picture down, nearest pixel, until its longer side is at
// most side.
func shrink(img image.Image, side int) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	longer := max(w, h)
	if longer <= side {
		return img
	}
	nw, nh := w*side/longer, h*side/longer
	out := image.NewGray(image.Rect(0, 0, nw, nh))
	for y := range nh {
		for x := range nw {
			out.Set(x, y, img.At(b.Min.X+x*w/nw, b.Min.Y+y*h/nh))
		}
	}
	return out
}
