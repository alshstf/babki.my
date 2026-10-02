// The longest free texts the server takes, in characters, as api/openapi.yaml
// declares them (maxLength) and the server enforces them. A field given one of
// these as its maxLength cannot be filled past what Save would be refused for.
// The browser counts UTF-16 units and the server code points, so the browser
// is the stricter of the two and never lets through what the server refuses.
export const MAX_NOTE = 1000;
export const MAX_ACCOUNT_NAME = 100;
export const MAX_INSTITUTION = 100;
export const MAX_PERSON_NAME = 100;
export const MAX_INSTRUMENT_NAME = 200;
export const MAX_TICKER = 32;
export const MAX_EVENT_SOURCE = 500;
export const MAX_EVENT_NOTE = 1000;
