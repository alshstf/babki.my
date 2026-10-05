// Package currency defines what is accepted as a currency code at a write.
//
// It checks the shape of an ISO 4217 code (three uppercase letters), not
// whether the code is assigned: the register changes independently of this
// program, and nothing here looks currencies up by code.
package currency

import "regexp"

// Pattern is the accepted shape. The OpenAPI document and the web forms state
// the same pattern; tests hold them to this constant.
const Pattern = `^[A-Z]{3}$`

var codeRe = regexp.MustCompile(Pattern)

// Valid reports whether code has the shape of a currency code.
func Valid(code string) bool { return codeRe.MatchString(code) }
