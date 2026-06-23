package feedback

import (
	"embed"
	"html"
	"net/http"

	"github.com/gin-gonic/gin"
)

//go:embed static/wooly-wave.png
var ratePageStatic embed.FS

const (
	ratePageSuccessTitle = "Thank you for your feedback!"
	ratePageSuccessBody  = "Your feedback is valuable to us and we will improve our product based on what you share."
	ratePageSuccessFoot  = "You can close this tab."
	woolyWaveAssetPath   = "/v1/feedback/assets/wooly-wave.png"
)

// WoolyWaveHandler serves the Wooly waving illustration bundled for the
// one-click rate confirmation page (resized from WorkWeave frontend/public).
func WoolyWaveHandler() gin.HandlerFunc {
	data, err := ratePageStatic.ReadFile("static/wooly-wave.png")
	if err != nil {
		panic("feedback: wooly-wave.png missing from embed: " + err.Error())
	}
	body := data
	return func(c *gin.Context) {
		c.Data(http.StatusOK, "image/png", body)
	}
}

// ratePageSuccess renders the branded confirmation after a one-click rating.
func ratePageSuccess() []byte {
	return ratePageLayout(ratePageLayoutParams{
		Title: ratePageSuccessTitle,
		Body:  ratePageSuccessBody,
		Foot:  ratePageSuccessFoot,
		Wooly: true,
	})
}

// ratePageError renders a branded dead-end page for malformed links, expired
// tokens, and persistence failures.
func ratePageError(message string) []byte {
	return ratePageLayout(ratePageLayoutParams{
		Title: "Something went wrong",
		Body:  message,
		Foot:  "You can close this tab.",
		Wooly: false,
	})
}

type ratePageLayoutParams struct {
	Title string
	Body  string
	Foot  string
	Wooly bool
}

func ratePageLayout(p ratePageLayoutParams) []byte {
	woolyBlock := ""
	if p.Wooly {
		woolyBlock = `<img class="wooly" src="` + woolyWaveAssetPath + `" alt="Wooly waving" width="176" height="176">`
	}
	return []byte(`<!doctype html><html lang="en"><head><meta charset="utf-8">` +
		`<meta name="viewport" content="width=device-width,initial-scale=1">` +
		`<meta name="robots" content="noindex">` +
		`<title>` + html.EscapeString(p.Title) + `</title>` +
		`<style>` + ratePageCSS + `</style></head><body>` +
		`<main class="card">` +
		`<div class="brand" aria-hidden="true"><span class="brand-mark"></span><span class="brand-name">Weave</span></div>` +
		`<h1>` + html.EscapeString(p.Title) + `</h1>` +
		`<p class="body">` + html.EscapeString(p.Body) + `</p>` +
		woolyBlock +
		`<p class="foot">` + html.EscapeString(p.Foot) + `</p>` +
		`</main></body></html>`)
}

const ratePageCSS = `
*,*::before,*::after{box-sizing:border-box}
body{
  margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;padding:1.5rem;
  font:16px/1.55 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;
  color:#0a0a0a;
  background:
    radial-gradient(ellipse 80% 60% at 50% -10%, rgba(236,99,65,.14), transparent 70%),
    linear-gradient(180deg,#fafafa 0%,#f3f4f6 100%);
}
.card{
  width:100%;max-width:26rem;padding:2rem 1.75rem 1.5rem;text-align:center;
  background:#fff;border:1px solid rgba(0,0,0,.06);border-radius:1.25rem;
  box-shadow:0 24px 48px -28px rgba(15,23,42,.28),0 1px 0 rgba(255,255,255,.8) inset;
}
.brand{display:inline-flex;align-items:center;gap:.5rem;margin-bottom:1.25rem}
.brand-mark{
  width:2rem;height:2rem;border-radius:.5rem;background:#EC6341;
  box-shadow:0 6px 16px -8px rgba(236,99,65,.65);
}
.brand-name{font-size:.8125rem;font-weight:600;letter-spacing:.06em;text-transform:uppercase;color:#737373}
h1{margin:0 0 .75rem;font-size:1.375rem;font-weight:650;line-height:1.25;letter-spacing:-.02em;color:#171717}
.body{margin:0 0 1.25rem;font-size:.9375rem;color:#525252}
.wooly{display:block;width:11rem;height:auto;margin:0 auto 1rem;object-fit:contain}
.foot{margin:0;font-size:.8125rem;color:#a3a3a3}
`
