# Event-link QR encoder

This dependency-free encoder generates QR Code Model 2 version 10 symbols in
byte mode with medium error correction. Its capacity is 213 bytes, sufficient
for the public event URLs. Output PNGs include at least four white modules on
each side and use integer pixel scaling to keep module edges sharp.

The QR matrix construction and Reed-Solomon algorithms are a focused Go
adaptation of [Project Nayuki's QR Code generator](https://github.com/nayuki/QR-Code-generator/blob/master/python/qrcodegen.py),
used under its MIT license, preserved in `LICENSE`. The application-specific
PNG renderer and API use only the Go standard library.

`go test ./internal/qrcode` checks image sizes, quiet zones, capacity, input
errors, and exact module equivalence with independent reference vectors from
Kazuhiko Arase's MIT-licensed JavaScript QRCode encoder, distributed in npm's
`qrcode-terminal` package. `testdata/golden.json` records SHA-256 hashes of each
row-major matrix, represented as ASCII `0`/`1`, for all eight allowed masks,
using `new QRCode(10, 0)` (version 10, medium error correction) and
`makeImpl(false, mask)`. The encoder may select any of these masks.

On macOS the tests also decode generated images through Apple's independent
CoreImage barcode reader. That check requires Xcode's Swift command-line tools
and usable graphics facilities. It first checks the reader against Apple's
own QR encoder, and skips if the platform or its sandbox prevents decoding.
