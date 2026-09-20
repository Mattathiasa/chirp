# Chirp design

Bold editorial direction: ink and paper, cobalt for my messages, tangerine for actions, butter/mint/pink/lilac for people and state. Display type is Bricolage Grotesque, body is Hanken Grotesk, data is JetBrains Mono.

- `prototype.html` is a single-file clickable prototype: landing page, key setup, the full chat app (trust flows, rooms, nearby, network, settings, message actions, handshake, Wi-Fi drop simulator) and a Desktop/Phone toggle. Open it in a browser. It loads the fonts from Google Fonts, so it needs internet for the exact look and falls back to system fonts offline.
- The daemon's own web UI (`internal/web/static`) uses the same tokens but with system fonts, because its Content-Security-Policy allows no external resources.

Rooms and file sharing appear in the prototype as previews. The Go daemon is one-to-one, text only.
