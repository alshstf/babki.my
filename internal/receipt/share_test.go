package receipt_test

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log/slog"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"github.com/makiuchi-d/gozxing"
	"github.com/makiuchi-d/gozxing/qrcode"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/category"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/apitest"
	"babki.my/babki/internal/platform/httpserver"
	"babki.my/babki/internal/platform/testdb"
	"babki.my/babki/internal/receipt"
)

// qrPhoto is a picture of a receipt's QR code, white around it as on paper.
func qrPhoto(t *testing.T, text string) []byte {
	t.Helper()
	m, err := qrcode.NewQRCodeWriter().Encode(text, gozxing.BarcodeFormat_QR_CODE, 400, 400, nil)
	if err != nil {
		t.Fatal(err)
	}
	img := image.NewGray(image.Rect(0, 0, m.GetWidth(), m.GetHeight()))
	for y := range m.GetHeight() {
		for x := range m.GetWidth() {
			if m.Get(x, y) {
				img.SetGray(x, y, color.Gray{})
			} else {
				img.SetGray(x, y, color.Gray{Y: 255})
			}
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// share is what «Поделиться» sends: files under «files», as the manifest says.
func share(t *testing.T, c *http.Client, url string, files map[string][]byte) *http.Response {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for name, data := range files {
		part, err := w.CreateFormFile("files", name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = part.Write(data)
	}
	_ = w.Close()
	req, err := http.NewRequest("POST", url+"/share", &body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Sec-Fetch-Site", "none")
	noFollow := *c
	noFollow.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := noFollow.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// A photo of a receipt shared from a phone: its QR code is read; with no row
// of its total the quick add opens with it, with one the receipt completes
// it. A statement shared lands on «Деньги» with what it brought; one not
// signed in is sent to sign in.
func TestAReceiptIsShared(t *testing.T) {
	pool := testdb.New(t)
	famStore := family.NewStore(pool)
	sm := family.NewSessionManager(pool)
	auth := family.NewAuth(sm, famStore)
	accStore, opStore := account.NewStore(pool), operation.NewStore(pool)
	conv := marketdata.NewConverter(marketdata.NewStore(pool))
	srv := httpserver.New(slog.Default(), pool)
	family.NewHandler(family.NewService(famStore), famStore, auth, sm).Mount(srv)
	account.NewHandler(accStore, famStore, conv, nil, auth, sm).Mount(srv)
	operation.NewHandler(operation.NewService(opStore), opStore, famStore, conv, auth, sm).Mount(srv)
	receipt.NewHandler(receipt.NewService(pool, opStore, accStore, category.NewStore(pool), operation.NewService(opStore)), auth, sm).Mount(srv)
	url, c := apitest.Serve(t, srv.Handler())

	photo := qrPhoto(t, "t=20261009T1915&s=2340.90&fn=7380440700123456&i=51243&fp=1234567890&n=1")
	if text, ok := receipt.ReadQR(photo); !ok || !strings.Contains(text, "fn=7380440700123456") {
		t.Fatalf("the photo's code = %q, %v", text, ok)
	}
	resp := share(t, c, url, map[string][]byte{"IMG_0001.png": photo})
	if loc := resp.Header.Get("Location"); resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(loc, "/?add&receipt=") {
		t.Errorf("a photo with no row = %d → %q, want the quick add with the receipt", resp.StatusCode, loc)
	}

	var acc struct {
		ID string `json:"id"`
	}
	apitest.Decode(t, apitest.Do(t, c, "POST", url+"/api/v1/accounts", `{"name":"Карта","type":"checking","currency":"RUB"}`), &acc)
	if r := apitest.Do(t, c, "POST", url+"/api/v1/operations", fmt.Sprintf(
		`{"account_id":%q,"type":"withdrawal","occurred_on":"2026-10-09","amount_minor":-50000,"currency":"RUB"}`, acc.ID)); r.StatusCode != http.StatusCreated {
		t.Fatalf("row = %d", r.StatusCode)
	}
	resp = share(t, c, url, map[string][]byte{"IMG_0002.png": qrPhoto(t, "t=20261009T1000&s=500.00&fn=1&i=2&fp=3&n=1")})
	if loc := resp.Header.Get("Location"); loc != "/money?shared=1.1.0.0.0.0" {
		t.Errorf("a photo with its row → %q, want «Деньги» with one completed", loc)
	}

	statement := `[{"ticket":{"document":{"receipt":{"dateTime":"2026-10-08T12:00:00","fiscalDriveNumber":"9","fiscalDocumentNumber":9,"totalSum":100}}}}]`
	resp = share(t, c, url, map[string][]byte{"checks.json": []byte(statement)})
	if loc := resp.Header.Get("Location"); loc != "/money?shared=1.0.1.0.0.0" {
		t.Errorf("a statement → %q", loc)
	}

	resp = share(t, c, url, map[string][]byte{"IMG_0004.png": []byte("not a picture")})
	if loc := resp.Header.Get("Location"); loc != "/money?shared=none" {
		t.Errorf("no receipt in it → %q", loc)
	}

	stranger := &http.Client{}
	resp = share(t, stranger, url, map[string][]byte{"IMG_0003.png": photo})
	if loc := resp.Header.Get("Location"); resp.StatusCode != http.StatusSeeOther || loc != "/login" {
		t.Errorf("not signed in = %d → %q, want the sign-in page", resp.StatusCode, loc)
	}
}
