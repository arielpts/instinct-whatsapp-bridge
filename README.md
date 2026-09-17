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
  leaves is the E.164 number, and only in the default mode. Identifiers live in
  headers, not only in prose, so routing never depends on parsing text.
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
  second WhatsApp message.

### 4.2 Security

- **SEC-1 — Explicit permissions.** Permissions are per conversation and per
  action (`read`, `draft`, `send`). No wildcards. Anything not granted is denied.
  Changing the allowlist requires editing config on the box and a restart —
  it is never changeable by email or by message.
- **SEC-2 — Authenticated email in both directions.** Forwards are DKIM-signed and
  carry an HMAC over their canonical fields inside the `Message-ID` (§7.1).
  Inbound mail is accepted only if DKIM verifies with a `d=` matching Instinct's
  sending domain *and* the `From` matches the single configured assistant
  address. Both, or the mail is dropped and logged. A threaded reply must
  additionally carry an `In-Reply-To` whose HMAC verifies and has not been seen
  before; a first-contact email without `In-Reply-To` is allowed but is never
  treated as a reply.
- **SEC-3 — Visible confirmation before sending.** Approval requests state the
  resolved recipient (display name *and* the identifier) and the exact bytes to
  be sent. No abbreviation, no rendering that could hide trailing content.
- **SEC-4 — Content is never control.** Routing is taken from envelope headers
  only. A message's recipient comes from the address the assistant replied to,
  never from text — so no string a third party can write, anywhere in a body, can
  redirect a message, name a different contact, or change a permission. Bodies
  are payload and nothing else: there is no command vocabulary to parse, and
  therefore none to inject.
- **SEC-11 — Catch-all hardening.** The bridge's domain accepts every local part
  by design, which invites dictionary spam. Mail failing SEC-2 is dropped without
  a bounce (a bounce confirms the address) and recorded as a count, not as
  content. Accepted mail is rate-limited independently of the send quotas.
- **SEC-12 — Nothing is sent that the assistant did not write.** Outbound text is
  extracted, never taken wholesale: quoted history, signatures and everything
  below the reply marker are stripped, HTML-only bodies are rejected rather than
  converted, and the result is scanned for our own identifiers and HMACs. Any
  ambiguity rejects the candidate. Mailers append; a bridge that forwards a whole
  body eventually quotes a contact's own message back at them, with the
  authenticator attached.
- **SEC-5 — Kill switch.** A single action stops all sending immediately:
  `touch PANIC` in the state directory, a `systemctl stop`, or an approval-channel
  command word. The switch fails closed — if the bridge cannot determine that
  sending is permitted, it does not send.
- **SEC-6 — Quotas.** Hard per-conversation and per-day send caps. Exceeding a cap
  is a hard stop requiring owner intervention, not a delay.
- **SEC-7 — Minimal retention.** Message bodies are retained for a configurable
  window (default 7 days) then purged; the audit log keeps identifiers, hashes
  and outcomes indefinitely, but not the content. Third parties' messages are
  never retained longer than needed to service a reply.
- **SEC-8 — Secrets on disk.** Credentials live in a file owned by the service
  user with mode `0600`, never in the repository, never in command-line
  arguments, never in the logs.
- **SEC-9 — Discard history sync.** On pairing, WhatsApp pushes recent history
  for *all* chats, not only allow-listed ones (whatsmeow surfaces this as
  `*events.HistorySync`). It arrives ahead of any filter the natural design would
  put in its way, so the handler drops it unconditionally and persists nothing.
  Otherwise the bridge's very first act violates the "never mirror the whole
  account" non-goal. Verified by test, not by inspection.
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
  ┌───────────────────┐   DKIM + sender; drop silently (SEC-2, SEC-11)
  │  verifier         │
  └────────┬──────────┘
           ▼
  ┌───────────────────┐   route by address; strip quotes (SEC-4, SEC-12)
  │  text extractor   │
  └────────┬──────────┘
           ▼
  ┌───────────────────┐   recipient + exact text shown to owner (SEC-3)
  │  approval queue   │ ◄──►  owner approves / rejects
  └────────┬──────────┘
           ▼
  ┌───────────────────┐   quotas, kill switch, dedup (SEC-5, SEC-6, FR-9)
  │  sender           │
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
Subject:    Marina (+55 11 98765-4321)
Message-ID: <m.0192bd4c.9f2ca817e3b4@wa.example.com>
X-WA-Message:   m_0192bd4c
X-WA-Timestamp: 2026-09-17T20:05:11Z
X-WA-Mode:      draft-only

--- reply above this line ---

oi, consegue me mandar o contrato ainda hoje?
```

The `Message-ID` is the authenticator: `m.<message id>.<HMAC>`, the HMAC taken
over conversation, message and timestamp under the box's key. A reply carries it
back in `In-Reply-To` for free, which is what binds the reply to this message and
what makes replay detectable (`SEC-2`, `FR-9`).

The `From` display name is the contact's name; the local part is the number. One
message, one email — no digests, so a reply can never be ambiguous about which
message it answers.

### 7.2 Reply (assistant → bridge)

Any mail to `<number>@wa.example.com` from the configured assistant address becomes
a candidate message to that number. A reply to a forward is the normal case; mail
with no `In-Reply-To` is accepted too, as a new message rather than a threaded
reply, and is subject to the same allowlist and the same approval.

The body is the message. Extraction is strict, because an email body is not just
what someone typed (`SEC-12`):

- Everything from the `--- reply above this line ---` marker down is discarded,
  along with quoted history (`>` blocks, `On … wrote:`) and signatures (`-- `).
- The remainder is trimmed and must be non-empty plain text. HTML parts are
  ignored in favour of `text/plain`; if only HTML exists, the candidate is
  rejected rather than converted.
- The result is scanned for our own identifiers — HMACs, `Message-ID`s, headers,
  nonces. A hit rejects the candidate. Quoting our forward back at the contact
  would leak the authenticator that lets someone forge the next one.
- Ambiguous extraction rejects. It never sends the whole body and hopes.

### 7.3 Status (bridge → assistant)

On the same thread: `status: sent | rejected | expired | failed`, with the new
message identifier on success, or the reason otherwise — including the
extraction failures above, so a rejected draft can be rewritten rather than
silently lost.

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

Recommended: a small **arm64 VPS**, Debian stable, ~€4–5/month (Hetzner CAX11 or
equivalent). arm64 matches the `CGO_ENABLED=0` cross-compile target, and the
cheapest tier is already several times the `OPS-1` budget. A Raspberry Pi at home
is equally viable technically — outbound-only means no router configuration — at
the cost of home power and network flaps, which cost linked-device sessions.
Free-tier ARM instances work but are evictable, and an evicted bridge is an
unlinked device.

Setup: one systemd unit, one service user, `0600` secrets file, `unattended-upgrades`,
default-deny inbound, SSH keys only. Disk stays a few hundred megabytes at default
retention.

**Pairing** is by QR (`GetQRChannel`), rendered as ASCII in the terminal over SSH
— which means re-pairing needs physical access to the phone, and is worth knowing
before the session drops at an inconvenient moment.

**Back up the device store.** `state.db` holds the linked-device session: lose it
and you re-pair from scratch; leak it and someone else holds a session on the
account. Encrypted, off-box, and the restore tested at least once before M4.

## 9. Configuration

```toml
mode           = "draft-only"   # draft-only | approve-each | approve-except
address_style  = "number"       # number | opaque  (§6.3)
retention_days = 7

[mail]
# our side: a catch-all on a dedicated subdomain
domain    = "wa.example.com"
mailbox   = "bridge@wa.example.com"      # receives the whole catch-all
imap_host = "imap.provider.net"
smtp_host = "smtp.provider.net"        # submission, port 587

# the assistant: the only sender we accept, the only recipient we send to
assistant_address     = "assistant@mail.instinct.com"
assistant_dkim_domain = "mail.instinct.com"   # DKIM d= must match

[limits]
per_conversation_per_hour = 5
per_day_total             = 30
inbound_mail_per_hour     = 60   # catch-all abuse cap (SEC-11)

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
- **Exit:** the approach and its risks are written down and accepted.

### M1 — Read-only forwarding *(the first rollout step)*
- [ ] whatsmeow: `sqlstore` on the pure-Go SQLite driver, QR pairing rendered as
      ASCII over SSH
- [ ] `*events.Message` handling for text; reconnect and keepalive survive a network drop
- [ ] `*events.HistorySync` discarded unconditionally, with a test (`SEC-9`)
- [ ] Read receipts and chat presence verified off (`SEC-10`)
- [ ] Allowlist filter with an empty default (`SEC-1`)
- [ ] Sign-up command: candidate generation, `IsOnWhatsApp` resolution, alias
      table, loud failure on zero or ambiguous hits (`FR-10`)
- [ ] `@c.us` / `@s.whatsapp.net` normalization on input (§6.1)
- [ ] Address ↔ conversation mapping, both `address_style` modes (`FR-3`, `FR-11`)
- [ ] SQLite schema + WAL; retention purge job (`SEC-7`)
- [ ] SMTP forward: number-as-sender, display name, signed `Message-ID`
      (`FR-2`, `SEC-2`)
- [ ] JSONL audit log (`OPS-3`)
- [ ] systemd unit, `0600` secrets file, service user (`OPS-2`, `SEC-8`)
- **Exit:** one allow-listed conversation forwards correctly for a week; no path
  in the binary can send anything.

### M2 — Drafting, still no sending
- [ ] IMAP poller over the catch-all; routing by recipient address (`FR-4`)
- [ ] Verifier: DKIM `d=` + sender identity; silent drop and counting (`SEC-2`, `SEC-11`)
- [ ] Text extraction: reply marker, quote and signature stripping, HTML-only
      rejection, identifier leak scan (`SEC-12`) — fuzzed against real mailer output
- [ ] Candidate queue persisted, with dedup (`FR-4`, `FR-9`)
- [ ] Status replies (`FR-7`)
- [ ] Draft-only enforcement proven by test, not by config (`FR-8`)
- **Exit:** drafts round-trip end to end; the send path is still absent.

### M3 — Sending, one test conversation
- [ ] Approval queue with one-time tokens and full recipient + text display (`SEC-3`, `FR-5`)
- [ ] Sender with quotas and hard-stop behaviour (`FR-6`, `SEC-6`)
- [ ] Kill switch: PANIC file, service stop, command word; fail-closed (`SEC-5`)
- [ ] Candidate expiry (default 15 min) and idempotent delivery
- [ ] Adversarial test suite: replayed `In-Reply-To`, forged sender, DKIM `d=`
      mismatch, mail to a non-allow-listed number, a contact whose message
      impersonates an instruction or an email header, ninth-digit variant of an
      allow-listed number, approval without token, body that quotes our own HMAC
- **Exit:** sending works in exactly one test conversation, with every
  adversarial test failing closed.

### M4 — Real use, narrow
- [ ] Add real conversations one at a time, each an explicit config change
- [ ] `bridge status` subcommand and local health endpoint (`OPS-4`)
- [ ] Session-loss alerting and degradation behaviour (`OPS-5`)
- [ ] Encrypted off-box backup of `state.db`, with a tested restore (§8.2)
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
4. Identity: WhatsApp is migrating addressing from phone-number JIDs to LIDs, and
   the two do not always map cleanly (WAHA carries open issues about exactly this
   mismatch). The allowlist matches on identity, so getting this wrong means
   either dropping wanted messages or, worse, matching an unintended one. Decide
   what the allowlist keys on, and how a JID/LID change is detected rather than
   silently re-matched.
5. **Blocking for M0:** can Instinct send from, and receive at,
   `assistant@mail.instinct.com`, and what DKIM `d=` domain does its outbound
   mail carry? SEC-2 pins the accepted sender to exactly that, so this must be
   observed from a real message rather than assumed.
6. Does Instinct's mail preserve `In-Reply-To` on replies? The whole binding and
   replay story rests on it. If it does not, the fallback is sub-addressing the
   HMAC into the local part (`5511987654321+9f2c…@wa.example.com`), which survives
   any mailer but is uglier.
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
