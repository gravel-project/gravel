// Package wardogs is a client for the War Dogs dedicated server's RCON API: plain HTTP, a bearer
// token, JSON bodies, and a configuration document in INI form (ADR-0010).
//
// War Dogs is in Early Access and its API changes every few weeks, so nothing here assumes a route
// exists. The server says what it serves at GET /v1/capabilities (public, like GET /v1/health);
// [Capabilities] matches those routes by shape, so a parameter renamed between builds ({id} versus
// {steamId}) is the same route and a route that is gone is a capability that is absent. A call
// whose route the server does not advertise fails with [ErrNotSupported] without a request.
// Parsing is tolerant: unknown fields are ignored and enum values are kept as strings.
//
// The server's rules a client must keep, all measured against the live server:
//
//   - Three requests with a missing or wrong token answer 429 on every protected route for a
//     while. The client sends one token and, after a 401, sends nothing protected again until
//     [Client.SetToken] gives it a new one ([ErrTokenRefused]); it never retries a 401.
//   - A 429 carries Retry-After. The client sends nothing until it has passed ([RateLimitedError]).
//   - 600 requests a minute per address and 64 KiB request bodies (the capabilities' limits).
//   - GET /v1/config returns the RCON password in clear. [RedactConfig] blanks it and the other
//     secrets before a document is stored, logged or committed.
//
// This package has no gravel imports: game clients become standalone modules at the open-source
// cut (ADR-0001), and depguard enforces it. gravel's driver in drivers/wardogs adapts it.
package wardogs
