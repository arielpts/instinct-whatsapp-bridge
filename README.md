# instinct-whatsapp-bridge

An email-based bridge that lets an AI assistant (Instinct) **read** selected WhatsApp
conversations and **draft replies** for them, with sending gated behind explicit
human approval.

The assistant has no WhatsApp integration of its own. This bridge supplies one:
a small self-hosted daemon that forwards allow-listed incoming messages to a
dedicated mailbox, and converts specially-formatted reply emails back into
WhatsApp messages — after the owner approves each one.

Designed to run on a **very small personal box** (1 vCPU / 512 MB–1 GB RAM,
Raspberry Pi class). Single static binary, SQLite, no external services beyond
an SMTP/IMAP account.

---

## 1. Motivation

The originating requirement, verbatim from the conversation that started this
project:

> "Quero conectar minhas conversas pessoais do WhatsApp para que o Instinct
> possa lê-las e responder por mim, com permissões claras por contato, conversa
> e tipo de ação. Também seria útil ter uma forma oficial e segura de integrar
> um servidor próprio, sem expor mensagens de terceiros nem permitir envios fora
> do escopo autorizado."

The assistant agreed this is workable as a bridge, on the condition that a
specific set of locks is mandatory. Those locks are **non-negotiable
requirements**, not features to be scheduled later:

| # | Lock (as stated) | Requirement ID |
|---|------------------|----------------|
| 1 | Explicit list of conversations it may act in | `SEC-1` |
| 2 | Unique conversation and message identifiers | `FR-3` |
| 3 | Recipient and final text clearly visible before sending | `SEC-3` |
| 4 | Signature / verification of the server's emails | `SEC-2` |
| 5 | Blocking of commands coming from message content | `SEC-4` |
| 6 | Log and a kill switch | `SEC-5`, `OPS-3` |

Rollout order is also fixed: **read + draft first**, sending enabled only later,
in a single test conversation.

## 2. Scope

**In scope**
- Forwarding inbound messages from an explicit allowlist of conversations.
- Receiving reply instructions by email and turning them into WhatsApp messages.
- Human approval of every outbound message (with a narrow, opt-in exception).
- Full audit log, quotas, kill switch.

**Out of scope (explicit non-goals)**
- Mirroring the entire WhatsApp account. Never the default, never a config flag.
- Reading or forwarding conversations not on the allowlist.
- Group conversations in v1 (third-party exposure is disproportionate).
- Media (images, audio, documents). v1 handles text and message metadata only.
- Any autonomous initiation of a conversation. The bridge only ever replies.
- Multi-tenant operation. One owner, one WhatsApp account, one box.

## 3. Threat model

The design assumes the following are hostile or unreliable:

- **Message content.** Anyone who can message the owner can write text that
  looks like an instruction. Message bodies are *data*, never commands.
- **The mailbox.** Email is unauthenticated by default. Anything arriving at the
  bridge's inbox is untrusted until cryptographically verified.
- **The assistant.** Not malicious, but it can be misled by content it reads and
  it can be wrong about recipients. It must never hold send authority alone.
- **The box.** A small personal VPS is not a hardened environment. Assume disk
  compromise is plausible; minimize what is stored and for how long.

Not defended against: a full compromise of the owner's own devices, or WhatsApp
itself. If the linked device session is stolen, the bridge's locks are moot.

**Legal / ToS note.** WhatsApp's official APIs (Cloud API / Business Platform)
cover *business* numbers only; there is no sanctioned API for a personal
account's conversations. This project links a personal account through
whatsmeow, an unofficial implementation of the multi-device protocol — the same
library WAHA's GOWS engine uses. Maturity of the library does not change the
status: it is against WhatsApp's Terms of Service and carries a real risk of
the number being banned. This project treats that as a decision for the owner to
make knowingly — it is documented here, not hidden, and `docs/adr/0001` must
record the choice before any code touches a real account. Use a secondary number
for all development.

## 4. Requirements

### 4.1 Functional

- **FR-1 — Allowlist-only ingestion.** The bridge forwards a message only if its
  conversation ID appears in the allowlist. Default allowlist is empty.
- **FR-2 — Forward as email.** Each allow-listed inbound message produces exactly
  one email to the dedicated mailbox, containing sender, timestamp, conversation
  identifier, message identifier, and body.
- **FR-3 — Stable identifiers.** Every conversation and message carries an
  identifier that is unique and stable across restarts. A conversation is
  identified by its email address, whose local part is the contact's number by
  default or an HMAC of the JID under `address_style = "opaque"` (§6.3). Message
  identifiers are opaque in both modes. The raw JID never leaves the box; what
  leaves is the E.164 number, and only in the default mode. Because the assistant
  cannot set arbitrary headers, the binding identifier must also survive in the
  `Subject` — so it is short, opaque and echo-friendly (`FR-13`).
- **FR-4 — Reply ingestion.** The bridge polls the catch-all mailbox, routes each
  accepted email by its recipient address, extracts the message text (§7.2), and
  enqueues a candidate outbound message.
- **FR-5 — Approval queue.** A candidate message is delivered only after the
  owner approves it, showing recipient and final text.
- **FR-6 — Send.** On approval, the message is sent to the named conversation as
  a reply to the referenced message where the protocol supports it.
- **FR-7 — Status feedback.** Every candidate resolves to a terminal state
  (`sent`, `rejected`, `expired`, `failed`) and that state is reported back by
  email on the original thread.
- **FR-8 — Draft-only mode.** A mode in which FR-6 is disabled entirely; the
  bridge accepts and logs drafts but can send nothing. This is the default mode
  and the mode used for the first rollout phase.
- **FR-9 — Deduplication.** A retried or replayed reply email must not produce a
  second WhatsApp message, and a partially delivered candidate is never
  auto-retried (§7.3).
- **FR-12 — Multi-bubble messages.** One candidate may carry several WhatsApp
  messages, separated in the body. Capped in count and length, approved as a
  unit, counted individually against quota, delivered in order (§7.3).
- **FR-13 — Subject token.** Every forward carries an opaque `[wa:…]` token in its
  subject, mapping to conversation and message on the box. The assistant controls
  `To`, `CC`, `BCC`, `Reply-To`, subject and standard threading, but cannot set
  arbitrary headers — so the token, not a custom header, is what a reply echoes
  back. It contains no phone number and no message content.

### 4.2 Security

- **SEC-1 — Explicit permissions.** Permissions are per conversation and per
  action (`read`, `draft`, `send`). No wildcards. Anything not granted is denied.
  Changing the allowlist requires editing the config file on the box and a
  restart — never by email, never by message, and never by an environment
  variable (§9.1).
- **SEC-2 — Authenticated email in both directions.** Forwards are DKIM-signed and
  carry an HMAC in both the subject token and the `Message-ID` (§7.1). Inbound
  mail is accepted only if every one of these holds: the `From` is the single
  configured assistant address; DKIM verifies with a `d=` on the Instinct
  allowlist (`SEC-14`); and the `[wa:…]` token verifies as an HMAC we issued and
  has not been redeemed before. Anything else is dropped and logged.
- **SEC-14 — Instinct sender allowlist.** The accepted sender is pinned by an
  explicit allowlist, not inferred: exact `From` addresses, permitted DKIM `d=`
  domains, permitted envelope/return-path domains, and — if Instinct publishes
  them — sending IP ranges. Empty by default; nothing is accepted until it is
  filled in from observed real mail.
  Authentication results are trusted only from the `Authentication-Results`
  header our own provider stamped on delivery. Any such header already present
  in the message is stripped before evaluation, because a sender can write
  those headers themselves. The stamp is matched on the provider's *domain*,
  not one hostname: observed in the wild as `mx13.migadu.com`, and nothing
  promises the next message arrives through mx13.
- **SEC-3 — Visible confirmation before sending.** Approval requests state the
  resolved recipient (display name *and* the identifier) and the exact bytes to
  be sent — every bubble, numbered, in order. No abbreviation, no rendering that
  could hide trailing content, no approving part of a candidate.
- **SEC-4 — Content is never control.** Routing is taken from envelope headers
  only. A message's recipient comes from the address the assistant replied to,
  never from text — so no string a third party can write, anywhere in a body, can
  redirect a message, name a different contact, or change a permission. Bodies
  are payload and nothing else: there is no command vocabulary to parse, and
  therefore none to inject.
- **SEC-11 — Catch-all hardening.** A domain that accepts every local part
  invites dictionary spam, so the pattern is narrowed at the provider first:
  conversation addresses are E.164 digits, and a rule matching only those refuses
  `info@` and the rest before anything is stored. What still arrives and fails
  SEC-2 is dropped without a bounce (a bounce confirms the address) and recorded
  as a count, not as content. Accepted mail is rate-limited independently of the
  send quotas.
- **SEC-12 — Nothing is sent that the assistant did not write.** Outbound text is
  extracted, never taken wholesale: quoted history, signatures and everything
  below the reply marker are stripped, HTML-only bodies are rejected rather than
  converted, and the result is scanned for our own identifiers and HMACs. Any
  ambiguity rejects the candidate. Mailers append; a bridge that forwards a whole
  body eventually quotes a contact's own message back at them, with the
  authenticator attached.
- **SEC-13 — Two-channel agreement.** A reply names its destination twice — the
  address it was sent to, and the token it echoes. They must resolve to the same
  conversation, and exactly one recipient may lie in our domain. Disagreement
  rejects; multiple recipients reject rather than fan out (§7.2).
- **SEC-5 — Kill switch.** A single action stops all sending immediately:
  `touch PANIC` in the state directory, a `systemctl stop`, or an approval-channel
  command word. The switch fails closed — if the bridge cannot determine that
  sending is permitted, it does not send.
- **SEC-6 — Quotas.** Hard per-conversation and per-day send caps, counted in
  bubbles rather than candidates. Exceeding a cap is a hard stop requiring owner
  intervention, not a delay.
- **SEC-7 — Minimal retention.** Message bodies are retained for a configurable
  window (default 7 days) then purged; the audit log keeps identifiers, hashes
  and outcomes indefinitely, but not the content. Third parties' messages are
  never retained longer than needed to service a reply.
- **SEC-8 — Secrets on disk.** Credentials reach the process through the
  environment, loaded by systemd from an `EnvironmentFile` owned by the service
  user with mode `0600` — never `Environment=` lines in the unit (unit files are
  world-readable and `systemctl show` prints them), never command-line arguments
  (`/proc/*/cmdline` is world-readable), never the repository, never the logs.
  The environment is inherited by child processes, so the bridge execs nothing.
- **SEC-9 — Discard history sync, then delete what the library kept.** On
  pairing, WhatsApp pushes recent history for *all* chats, not only
  allow-listed ones (whatsmeow surfaces this as `*events.HistorySync`). The
  handler drops the payload unconditionally, so none of it is ever read.
  That is not sufficient on its own, and the first real pairing proved it:
  whatsmeow processes the payload **before** our handler runs and persists
  parts of it to the device store — message secret keys, privacy tokens, and
  push names for every contact it saw, two thousand of them on a live account.
  Discarding the event stops us reading those names; it does not stop them
  being on disk. So contact rows for anyone not allow-listed are deleted after
  every sync (`wa-bridge prune`, and automatically after pairing), and the
  operator is told the count rather than asked to take it on trust.
  Crypto material for unread chats is left alone; names are the identifying
  part and names are what goes.
- **SEC-10 — Reading leaves no trace.** Read receipts and typing indicators are
  never emitted. Both are visible to the third party, and marking messages read
  silently alters the owner's own unread state on their phone. Reading is
  observation only.

### 4.3 Operational

- **OPS-1 — Resource budget.** Steady-state RSS ≤ 150 MB, idle CPU ≈ 0%, disk
  footprint ≤ 500 MB including the database at default retention.
- **OPS-2 — Single unit.** One systemd service, one binary, one SQLite file.
  Restart-safe: a crash mid-flight loses no approved message and sends no
  unapproved one.
- **OPS-3 — Audit log.** Append-only, structured (JSONL), one record per decision:
  forwarded, verification failure, candidate created, approved, rejected, sent,
  quota hit, kill switch engaged.
- **OPS-4 — Observability on a small box.** Health via a local-only HTTP endpoint
  and a `bridge status` subcommand. No metrics stack required.
- **OPS-5 — Recovery.** Losing the WhatsApp session must degrade to "forwards
  stop, nothing is sent" and alert the owner, never to silent inactivity.

## 5. Architecture

```
  WhatsApp (linked device)
          │  inbound message
          ▼
  ┌───────────────────┐
  │  whatsmeow client │  multi-device; receive only, history sync dropped
  └────────┬──────────┘
           ▼
  ┌───────────────────┐   drop if conversation not allow-listed (SEC-1)
  │  allowlist filter │
  └────────┬──────────┘
           ▼
  ┌───────────────────┐   resolve JID → address, sign Message-ID (FR-3, FR-11, SEC-2)
  │  normalizer       │
  └────────┬──────────┘
           ▼
  ┌───────────────────┐
  │  mailer (SMTP)    │ ──────────────►  dedicated mailbox  ──►  Instinct reads
  └───────────────────┘
                                                │
  ┌───────────────────┐                         │  reply email
  │  poller (IMAP)    │ ◄───────────────────────┘  catch-all
  └────────┬──────────┘
           ▼
  ┌───────────────────┐   allowlist + DKIM; drop silently (SEC-2, SEC-14)
  │  verifier         │
  └────────┬──────────┘
           ▼
  ┌───────────────────┐   token + address must agree (SEC-13)
  │  text extractor   │
  └────────┬──────────┘
           ▼
  ┌───────────────────┐   recipient + exact text shown to owner (SEC-3)
  │  approval queue   │ ◄──►  owner approves / rejects
  └────────┬──────────┘
           ▼
  ┌───────────────────┐   quotas, kill switch, dedup (SEC-5, SEC-6, FR-9)
  │  sender (bubbles) │
  └────────┬──────────┘
           ▼
     WhatsApp send          →  audit log (OPS-3) at every arrow
```

**Stack.** Go, single static binary, [`whatsmeow`](https://github.com/tulir/whatsmeow)
(`go.mau.fi/whatsmeow`) for the WhatsApp side, SQLite (WAL) for state,
`net/smtp` + an IMAP client for mail.

### 5.1 WhatsApp client layer

The client layer is **whatsmeow** — the Go multi-device library that
[WAHA](https://github.com/devlikeapro/waha) runs underneath its **GOWS** engine,
and the same library behind `mautrix-whatsapp`. WAHA describes GOWS as the
browser-free, Go, "future replacement for NOWEB" alongside its two older engines,
WEBJS (whatsapp-web.js driving a headless Chromium) and NOWEB (Baileys on Node).

We take the library, not the wrapper. WAHA is a Dockerised REST service; running
it means a container plus an HTTP layer plus our bridge on top, and our bridge
would then re-implement the allowlist and the send gate *above* an API that is
itself capable of sending anywhere. Linking whatsmeow directly means the send
path exists only inside the binary that owns the locks, and the box runs one
process instead of three. WAHA remains the reference implementation to read when
the protocol misbehaves.

Why this over the alternatives, for this box specifically: WEBJS needs a headless
Chromium, which is several hundred megabytes of RSS before a message is
processed and is not a serious proposition on a 512 MB machine; NOWEB/Baileys
drops the browser but keeps a Node runtime and a large dependency tree to patch
on a machine nobody is watching. A static Go binary is ~40–80 MB resident with
no runtime to install.

Build note: whatsmeow's `sqlstore` upstream examples use the cgo `sqlite3`
driver. We register the pure-Go `modernc.org/sqlite` driver instead so the
binary builds with `CGO_ENABLED=0` and cross-compiles to arm64 from anywhere.
Foreign keys must be on (`?_foreign_keys=on`) either way.

Concretely, from whatsmeow: `sqlstore.New` → `GetFirstDevice` for the device
store, `GetQRChannel` or `PairPhone` to link, `AddEventHandler` for
`*events.Message`, and `SendMessage` with a `ContextInfo` carrying the quoted
message key for threaded replies.

Two of its behaviours are security-relevant, and both are requirements rather
than settings: the `*events.HistorySync` payload delivered at pairing is dropped
unconditionally (`SEC-9`), and `MarkRead` / `SendChatPresence` stay off so that
reading leaves no trace on the account (`SEC-10`).

**Approval channel.** v1 uses the owner's own WhatsApp chat with the bridge
(the same chat the assistant already talks in), because it needs no extra
infrastructure. Approval requires a fresh one-time token that the bridge issues;
an approval message without a valid token is ignored, which keeps SEC-4 intact
even on the approval path.

## 6. Identity and addressing

### 6.1 The identity we trust

WhatsApp's own JID is the only identity the bridge trusts. We never build one out
of a phone number and assume it is right.

| Form | Where it comes from | Our handling |
|---|---|---|
| `5511987654321@s.whatsapp.net` | whatsmeow, multi-device | canonical internal form |
| `5511987654321@c.us` | whatsapp-web.js, WAHA's WEBJS, most Brazilian tooling | accepted on input, normalized on the way in |
| `<opaque>@lid` | WhatsApp's newer addressing | stored alongside the JID; see §11 |
| `…@g.us` | groups | rejected in v1 (non-goal) |

`@c.us` and `@s.whatsapp.net` name the same individual-user address space; only
the era of the client differs. Sign-up accepts either, and anything we print or
store is the canonical form.

### 6.2 The Brazilian ninth digit

Brazil added a ninth digit to mobile numbers, and WhatsApp accounts registered
before that rollout can still be addressed without it. The same person is
therefore reachable as both `5511987654321` and `551187654321`, and only one of
those is the account's real JID. Guessing gives you a message that silently goes
nowhere, or a second conversation with someone you are already talking to — and
under an allowlist, a mismatched identity is also a lock that fails open or shut
for the wrong person.

The widely repeated heuristic (keep the ninth digit for DDDs up to 30, drop it
above) is used for exactly one purpose: generating candidates. It is never
treated as the answer.

**FR-10 — Number resolution at sign-up.**
1. Normalize to E.164 digits: strip `+`, spaces, parentheses, dashes.
2. Build the candidate set — the number as given, plus, for `55` mobiles, the
   variant with the ninth digit added or removed.
3. Ask WhatsApp. `IsOnWhatsApp` over the candidates returns the JID that exists;
   that is the truth, and the heuristic's job ends here.
4. Store the resolved JID, and the whole candidate set as aliases.
5. Zero hits, or more than one distinct JID, fails the sign-up loudly and a human
   decides. There is no silent fallback.

**FR-11 — Alias collapse.** Every alias resolves to one conversation. Mail
addressed to `5511987654321@…` and to `551187654321@…` reaches the same chat and
the same allowlist entry. Outbound mail always uses the canonical form, so
threads stay stable in the mailbox.

Resolution runs at sign-up and is cached. It is not re-run per message — a
contact-existence query per inbound message is both slow and a recognisable
automation pattern.

### 6.3 What the phone-number convention costs

Using the number as the email local part (§7) is a deliberate choice: it makes
the mailbox readable, makes replying obvious, and needs no lookup table to use by
hand. It also means third parties' phone numbers travel in email headers and come
to rest in a mailbox on someone else's servers, indefinitely, for people who
never agreed to any of this.

That is the owner's call to make and it is made: **number-as-address is the
default**. `address_style = "opaque"` switches the local part to an HMAC of the
JID, with the display name still carrying the human name, and the rest of the
protocol is unchanged — so this can be revisited without a redesign. `FR-3`
above is written to match: message identifiers stay opaque either way, and the
raw JID never leaves the box in either mode.

## 7. Email protocol

Routing lives in the envelope, not in the body. A conversation *is* an email
address; replying to it sends a WhatsApp message. The assistant does not have to
learn a command language, and — more importantly — the recipient of an outbound
message comes from a header the assistant controls, never from text that a third
party could have written.

```
WhatsApp contact  ↔  5511987654321@wa.example.com  ↔  assistant@mail.instinct.com
```

### 7.1 Forward (bridge → assistant)

```
From:       "Marina" <5511987654321@wa.example.com>
To:         assistant@mail.instinct.com
Subject:    [wa:7f3a91c2] Marina (+55 11 98765-4321)
Message-ID: <m.0192bd4c.9f2ca817e3b4@wa.example.com>
X-WA-Message:   m_0192bd4c          <- convenience only, never depended on
X-WA-Timestamp: 2026-09-17T20:05:11Z
X-WA-Mode:      draft-only

--- reply above this line ---

oi, consegue me mandar o contrato ainda hoje?
```

**The subject token is the binding.** `[wa:7f3a91c2]` is a truncated HMAC over
conversation, message and timestamp under the box's key, resolved through a local
map. It is opaque: no phone number, no content, nothing that means anything off
this box. It goes in the subject because that is what the assistant can reliably
read and reliably echo.

The `X-WA-*` headers are there for debugging and for any other client, and
nothing depends on them. The assistant can set `To`, `CC`, `BCC`, `Reply-To` and
standard threading, but **not arbitrary headers** — so a protocol that needed
`X-WA-Conversation` on the way back would not work at all. `Message-ID` still
carries the same HMAC, so `In-Reply-To` corroborates the token when threading
survives, but it is never the only binding.

The `From` display name is the contact's name; the local part is the number. One
message, one email — no digests, so a reply is never ambiguous about which
message it answers.

### 7.2 Reply (assistant → bridge)

A reply names its destination twice, and the two must agree:

| Channel | Carries | Source |
|---|---|---|
| `X-Envelope-To` | which contact | the address the mail was delivered for, stamped by the provider |
| `[wa:…]` token | which conversation and message | echoed from the subject, or the first body line |

The envelope header, not `To:`. The catch-all delivers every conversation's
address into one mailbox, so `Delivered-To` says only which mailbox it landed
in — confirmed on a real delivery, where it read `bridge@` while
`X-Envelope-To` read `5541996616614@`. `To:` happens to agree, but the sender
writes it and may list several addresses; the envelope header they do not
write. Both headers can be forged further down the message, so only the first
occurrence counts, the provider having prepended its own on delivery.

**SEC-13 — Two-channel agreement.** The token is authoritative and the address is
the cross-check. If the token resolves to one conversation and the address to
another, the candidate is rejected — never silently preferred one way. Sending to
the wrong person then requires two independent failures rather than one typo.
Exactly one recipient may lie in our domain; a reply addressed to two contacts is
rejected outright rather than fanned out, because quiet fan-out is precisely the
"envio fora do escopo" this project exists to prevent.

The token is read from the `Subject`, or failing that from the first non-empty
line of the body. A token found anywhere else is ignored — and, since it should
not be there, treated as a leaked-identifier rejection under `SEC-12`. Tokens are
verified as HMACs, not merely looked up, so a token appearing inside quoted
contact text cannot resolve to anything.

Mail with a valid token but no `In-Reply-To` is normal, not suspicious. Mail with
neither token nor thread is accepted only as a first message to an allow-listed
address, and never qualifies for any standing authorization.

The body is the message. Extraction is strict, because an email body is not just
what someone typed (`SEC-12`):

- Everything from the `--- reply above this line ---` marker down is discarded,
  along with quoted history (`>` blocks, `On … wrote:`) and signatures (`-- `).
- The remainder is trimmed and must be non-empty plain text. HTML parts are
  ignored in favour of `text/plain`; if only HTML exists, the candidate is
  rejected rather than converted.
- The result is scanned for our own identifiers — HMACs, `Message-ID`s, header
  names, tokens. A hit rejects the candidate. Quoting our forward back at the
  contact would leak the authenticator that signs the next one.
- Ambiguous extraction rejects. It never sends the whole body and hopes.

### 7.3 Bubbles

WhatsApp is written in several short messages, not one paragraph, and the
assistant can split a reply to match. A body may carry separators:

```
[wa:7f3a91c2]
---bubble---
Consigo sim.
---bubble---
Te mando até as 18h.
```

**FR-12 — Multi-bubble messages.** A line that is exactly `---bubble---` splits
the extracted text; no separator means one bubble. Each bubble is trimmed and
must be non-empty, there is a hard cap on count (default 5) and on length, and
exceeding either rejects the whole candidate rather than truncating it.

The separator is parsed only in text that survived extraction, never in
forwarded contact content — a contact who writes `---bubble---` at you cannot
reach the splitter, and even if they could, splitting changes nothing about
where a message goes (§7.2 settles that).

The consequences are spelled out because "one candidate, many messages" breaks
several earlier assumptions:

- **Approval** shows every bubble, numbered, in order, exactly as it will be
  sent. Approval is all-or-nothing; there is no approving bubble 1 of 3 (`SEC-3`).
- **Quotas** count bubbles, not candidates. Five bubbles spend five (`SEC-6`).
- **Delivery** is sequential, with each bubble's state recorded before and after
  it goes out, and a short pause between them so a reply does not land as a burst.
- **Partial failure** stops. If bubble 3 of 5 fails, the candidate ends
  `partial`, reports how many were delivered, and is never auto-retried — a retry
  of a partially-sent candidate duplicates the bubbles that already arrived
  (`FR-9`).

### 7.4 Status (bridge → assistant)

On the same thread, subject token included so it can be matched without headers:
`status: sent | partial | rejected | expired | failed`, with the new message
identifiers on success, the delivered count on `partial`, and the reason
otherwise — including the extraction and agreement failures above, so a rejected
draft can be rewritten rather than silently lost.

## 8. Deployment

### 8.1 Domain and mail

Two mail identities, one on each side:

| | Address | Provided by |
|---|---|---|
| Bridge → assistant | `<number>@wa.example.com` | a **catch-all** mailbox on a subdomain you control |
| Assistant → bridge | `assistant@mail.instinct.com` | Instinct |

A dedicated subdomain, not the apex domain itself: every conversation needs its own
address, so the domain must accept a catch-all, and you do not want a catch-all
on the domain that carries real mail. Delegating `wa.example.com` also keeps its
SPF/DKIM/DMARC records — and its sending reputation — separate from anything
else that domain does.

DNS on `wa.example.com`: MX to the provider, SPF, provider DKIM key, and
`DMARC p=reject`. The subdomain sends to exactly one recipient and should never
be spoofable.

**Use a managed mail provider, not the box.** Most VPS hosts block outbound port
25, a fresh IP has no sending reputation, and a small unattended machine has no
business running an internet-facing SMTP daemon. Any provider with per-domain
catch-all plus IMAP and SMTP submission works — Migadu, Fastmail, Mailbox.org,
Zoho, Purelymail. The bridge only makes outbound connections: IMAP to read the
catch-all, SMTP submission on 587 to send, and WSS to WhatsApp. No inbound ports,
no static IP, no port forwarding.

A catch-all attracts dictionary spam by design, hence **SEC-11**: mail is
accepted only from the one configured assistant address with a DKIM `d=` matching
Instinct's sending domain. Everything else is dropped without a bounce — bouncing
confirms the address exists — and logged as a count, not as content.

### 8.2 The box

Constraint: a linked device that stays offline long enough gets unlinked, so this
must be genuinely always-on. That rules out a laptop.

Recommended: a small **arm64 VPS**, Debian stable — Hetzner CAX11, €5.99/month
plus €0.50 for the primary IPv4 since the June 2026 price adjustment. arm64
matches the `CGO_ENABLED=0` cross-compile target and is now the value tier
outright, that adjustment having raised dedicated-vCPU plans far more steeply.
Note that Hetzner offers arm64 only in Falkenstein, Nuremberg and Helsinki, so a
box nearer Brazil means x86 and a larger bill. Either way the cheapest tier is
several times the `OPS-1` budget. Step-by-step provisioning is in
[`deploy/`](deploy/). A Raspberry Pi at home
is equally viable technically — outbound-only means no router configuration — at
the cost of home power and network flaps, which cost linked-device sessions.
Free-tier ARM instances work but are evictable, and an evicted bridge is an
unlinked device.

Setup: one systemd unit, one service user, `0600` secrets file, `unattended-upgrades`,
default-deny inbound, SSH keys only. Disk stays a few hundred megabytes at default
retention.

**Pairing** is by code, not by QR. `PairPhone` returns eight characters to type
into WhatsApp under Linked Devices; a QR rendered in a terminal assumes a second
screen to scan it from, which an operator working from the phone that runs
WhatsApp does not have. QR remains available for anyone with a laptop to hand.
Either way the phone itself is required, and the login socket closes after about
160 seconds — both worth knowing before the session drops at an inconvenient
moment.

**Back up the device store.** `state.db` holds the linked-device session: lose it
and you re-pair from scratch; leak it and someone else holds a session on the
account. Encrypted, off-box, and the restore tested at least once before M4.

## 9. Configuration

Two sources, split by kind. **The environment carries deployment identity and
secrets; the file carries policy.** Which domain we are and which mailbox we log
into changes per box and must not sit in the repository; who may be messaged and
under what limits is the substance of the thing and belongs in a file that can be
reviewed and diffed.

### 9.1 Environment

```sh
WA_BRIDGE_MAIL_DOMAIN=wa.example.com          # the catch-all subdomain
WA_BRIDGE_MAILBOX=bridge@wa.example.com       # receives the whole catch-all
WA_BRIDGE_ASSISTANT_ADDRESS=assistant@mail.instinct.com
WA_BRIDGE_IMAP_HOST=imap.provider.net
WA_BRIDGE_IMAP_USER=…
WA_BRIDGE_IMAP_PASSWORD=…                    # secret
WA_BRIDGE_SMTP_HOST=smtp.provider.net       # submission, 587
WA_BRIDGE_SMTP_USER=…
WA_BRIDGE_SMTP_PASSWORD=…                    # secret
WA_BRIDGE_HMAC_KEY=…                        # secret; signs tokens and Message-IDs
WA_BRIDGE_STATE_DIR=/var/lib/wa-bridge
WA_BRIDGE_CONFIG=/etc/wa-bridge/config.toml
```

**The environment may tighten, never loosen.** `WA_BRIDGE_MODE=draft-only` can
force draft-only over a file that permits sending; no variable can enable
sending, add a conversation, raise a quota, or widen the sender allowlist. Policy
moves in one direction from the environment, because a drop-in env file is the
easiest thing on the box to change by accident and the hardest to notice.

Startup fails closed: an unset or malformed domain, mailbox or HMAC key refuses
to start rather than defaulting. The resolved domain is logged once at startup so
a misconfiguration is visible in the first line of the journal rather than in a
message that went somewhere unexpected.

The mail domain is recorded in `state.db` on first run. If it later changes,
tokens issued under the old domain are refused rather than redeemed, and
in-flight candidates are expired — addresses and tokens are scoped to a domain,
and carrying them across one is a replay waiting to happen.

### 9.2 Configuration file

```toml
mode           = "draft-only"   # draft-only | approve-each | approve-except
address_style  = "number"       # number | opaque  (§6.3)
retention_days = 7

# Mail hosts, domain, mailbox and credentials come from the environment (§9.1).

# SEC-14 — empty until filled in from observed real mail; nothing is accepted
# while it is empty. authserv_id names OUR provider, the only stamp we trust.
[instinct]
from_addresses   = ["assistant@mail.instinct.com"]
dkim_domains     = ["mail.instinct.com"]
envelope_domains = ["mail.instinct.com"]
ip_ranges        = []                      # only if Instinct publishes them
authserv_domain  = "provider.net"

[limits]
per_conversation_per_hour = 5    # counted in bubbles, not candidates
per_day_total             = 30
inbound_mail_per_hour     = 60   # catch-all abuse cap (SEC-11)
bubbles_per_candidate     = 5
chars_per_bubble          = 4096
bubble_pause_ms           = 1200

[[conversation]]
number  = "+55 11 98765-4321"                # as a human writes it
jid     = "5511987654321@s.whatsapp.net"     # resolved at sign-up, never guessed
aliases = ["5511987654321", "551187654321"]  # both ninth-digit forms (FR-11)
label   = "Trabalho — Marina"
actions = ["read", "draft"]                  # "send" added only in phase M3
```

`jid` and `aliases` are written by the sign-up command, not by hand — a
hand-written JID is a guess, and §6.2 exists because guesses here are wrong often
enough to matter.

`[instinct].from_addresses` is the list of senders we *accept*;
`WA_BRIDGE_ASSISTANT_ADDRESS` is the single address we *send to*. Different
roles, and the accept side stays in reviewable policy rather than in the
environment.

## 10. Roadmap and tasks

Phases are gates, not suggestions: phase N+1 does not begin until phase N's exit
criterion holds.

### M0 — Decisions and skeleton
- [ ] ADR 0001: record the WhatsApp-access decision and its ToS/ban risk, signed off by the owner
- [ ] ADR 0002: whatsmeow (the library behind WAHA's GOWS engine) + SQLite, with
      the small-box rationale and the reasons for embedding it rather than running WAHA
- [ ] Repository skeleton, `Makefile`, static build, CI that builds for arm64 and amd64
- [ ] `PROTOCOL.md` promoted out of §7 into its own normative document
- [ ] Provision `wa.example.com`: MX, catch-all mailbox, SPF, DKIM, `DMARC p=reject`
- [ ] Confirm `assistant@mail.instinct.com` exists, and pin Instinct's DKIM `d=`
      domain — SEC-2 cannot be implemented without it (§11)
- [ ] Provision the box: arm64 VPS, Debian, service user, unattended-upgrades
- [ ] Config loader: environment for identity and secrets, file for policy, with
      the tighten-only rule and fail-closed startup validation (§9)
- **Exit:** the approach and its risks are written down and accepted.

### M1 — Read-only forwarding *(the first rollout step)*
- [x] whatsmeow: `sqlstore` on the pure-Go SQLite driver, pairing by typed code
- [x] `*events.Message` handling for text; reconnect and keepalive survive a network drop
- [x] `*events.HistorySync` discarded unconditionally, with a test (`SEC-9`)
- [x] Prune contacts on a schedule inside `run`, not only after pairing.
      Measured on a live account: 3,306 contact rows after one pair. Every
      reconnect re-syncs, so a one-off prune is a one-off reprieve (`SEC-9`)
- [x] Read receipts and chat presence verified off (`SEC-10`)
- [x] Allowlist filter with an empty default (`SEC-1`)
- [x] Sign-up command: candidate generation, `IsOnWhatsApp` resolution, alias
      table, loud failure on zero or ambiguous hits (`FR-10`)
- [x] `@c.us` / `@s.whatsapp.net` / `@lid` normalization on input (§6.1)
- [x] Address ↔ conversation mapping (`FR-3`, `FR-11`)
- [x] SQLite schema + WAL; retention purge job (`SEC-7`)
- [x] SMTP forward: number-as-sender, display name, subject token, signed
      `Message-ID` (`FR-2`, `FR-13`, `SEC-2`)
- [x] Token issue/redeem map with single-use semantics
- [x] Retry unforwarded messages. Recording precedes sending, so a failed send
      stranded the message where nothing would look at it again.
- [ ] JSONL audit log (`OPS-3`)
- [ ] systemd unit installed and enabled (`OPS-2`, `SEC-8`)
- **Exit:** one allow-listed conversation forwards correctly for a week; no path
  in the binary can send anything. *First live forward: 2026-09-18. The
  binary has no WhatsApp send path at all yet, so the second half holds
  trivially; the week is running.*

### M2 — Drafting, still no sending
- [ ] IMAP poller over the catch-all; routing by recipient address (`FR-4`)
- [ ] Instinct sender allowlist; trust only our provider's `Authentication-Results`
      by `authserv-id`, stripping any pre-existing copies (`SEC-14`)
- [ ] Verifier: allowlist + DKIM + token redemption; silent drop and counting
      (`SEC-2`, `SEC-11`)
- [ ] Two-channel agreement and single-recipient enforcement (`SEC-13`)
- [ ] Bubble splitting with count and length caps (`FR-12`)
- [ ] Text extraction: reply marker, quote and signature stripping, HTML-only
      rejection, identifier leak scan (`SEC-12`) — fuzzed against real mailer output
- [ ] Candidate queue persisted, with dedup (`FR-4`, `FR-9`)
- [ ] Status replies (`FR-7`)
- [ ] Draft-only enforcement proven by test, not by config (`FR-8`)
- **Exit:** drafts round-trip end to end; the send path is still absent.

### M3 — Sending, one test conversation
- [ ] Approval queue with one-time tokens and full recipient + text display (`SEC-3`, `FR-5`)
- [ ] Sender with quotas in bubbles and hard-stop behaviour (`FR-6`, `SEC-6`)
- [ ] Sequential bubble delivery, per-bubble state, `partial` on failure, no
      auto-retry (`FR-12`, `FR-9`)
- [ ] Kill switch: PANIC file, service stop, command word; fail-closed (`SEC-5`)
- [ ] Candidate expiry (default 15 min) and idempotent delivery
- [ ] Adversarial test suite: replayed `In-Reply-To`, forged sender, DKIM `d=`
      mismatch, mail to a non-allow-listed number, a contact whose message
      impersonates an instruction or an email header, ninth-digit variant of an
      allow-listed number, approval without token, body that quotes our own HMAC,
      token/address disagreement, reply addressed to two contacts, replayed
      token, contact text containing `---bubble---`
- **Exit:** sending works in exactly one test conversation, with every
  adversarial test failing closed.

### M4 — Real use, narrow
- [ ] Add real conversations one at a time, each an explicit config change
- [ ] `bridge status` subcommand and local health endpoint (`OPS-4`)
- [ ] Session-loss alerting and degradation behaviour (`OPS-5`)
- [ ] Encrypted off-box backup of `state.db`, with a tested restore (§8.2)
- [ ] Deliverability. The first live forwards landed in Gmail's spam folder: a
      new subdomain with no sending reputation, a numeric local part and no
      prior correspondence. Confirm SPF, DKIM and DMARC all pass and align,
      then let reputation build. A forward in a spam folder is a forward the
      assistant never sees, and nothing in the bridge would report it — the
      send succeeded.
- [ ] Resource measurement against the `OPS-1` budget on the target box
- **Exit:** running unattended for two weeks within budget, zero unapproved sends.

### M5 — Later, if ever
- [ ] `approve-except` mode: per-conversation standing authorization for narrow
      cases, still logged, still quota-bound
- [ ] Media handling
- [ ] Group conversations, only with a defensible third-party story

## 11. Open questions

1. Which number is used for development? A secondary number is assumed; confirm.
2. Approval channel: the WhatsApp control chat is the zero-infrastructure option,
   but it puts approval on the same medium as the content. Is a separate channel
   (a local web page over Tailscale, say) worth the extra moving part?
3. Standing authorizations (`approve-except`) were left open in the original
   conversation. Which concrete case justifies one, if any?
4. *(Settled by observation.)* `IsOnWhatsApp` answers with a **LID**
   (`270565893996711@lid`), not a phone-number JID, for a number that plainly
   has one. A LID identifies the account and says nothing about the number, so
   the allowlist carries both — the LID and both ninth-digit spellings — since a
   chat may be addressed either way, and the email address is built from the
   configured number rather than from the identifier. What remains open is
   whether a LID can change for a stable account; if it can, a conversation
   would silently stop matching.
5. **Blocking for M0:** can Instinct send from, and receive at,
   `assistant@mail.instinct.com`, and what DKIM `d=` domain does its outbound
   mail carry? SEC-2 pins the accepted sender to exactly that, so this must be
   observed from a real message rather than assumed.
6. Does Instinct publish sending IP ranges, or a stable envelope domain, to make
   `SEC-14` tighter than an address plus a DKIM `d=`? Until then the allowlist
   starts empty and is filled from a real message.
   *(Settled: arbitrary headers are impossible on the assistant's side, and
   `In-Reply-To` is no longer load-bearing — the subject token carries the
   binding, which is why `FR-13` exists.)*
7. Reply-marker discipline: assistant replies that quote the forward are handled
   by §7.2, but a mailer that indents rather than quotes will defeat stripping.
   Worth observing what Instinct's mail actually looks like before finalizing the
   extractor.
8. Retention default: is 7 days right, or should content purge as soon as a
   candidate reaches a terminal state?

## 12. References

- [`tulir/whatsmeow`](https://github.com/tulir/whatsmeow) — the client library
  ([package docs](https://pkg.go.dev/go.mau.fi/whatsmeow),
  [`store/sqlstore`](https://pkg.go.dev/go.mau.fi/whatsmeow/store/sqlstore))
- [`devlikeapro/waha`](https://github.com/devlikeapro/waha) (Apache-2.0) — the
  GOWS engine's use of whatsmeow is the reference implementation for protocol
  behaviour; [engine comparison](https://waha.devlike.pro/docs/how-to/engines/)

## 13. Status

Pre-implementation. This README is the specification; nothing in M0 is done yet.
