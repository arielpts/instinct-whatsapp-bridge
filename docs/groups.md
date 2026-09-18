# Group conversations

Status: **proposed**. Groups are a non-goal in the current build (README §2) and
nothing here is implemented. This document says what would have to be true to
change that.

## Why they were excluded

Every other decision in this project is about one person's messages reaching one
assistant under that person's control. A group breaks that shape.

A two-party chat exposes one third party, who is already talking to the owner. A
group of forty exposes forty, most of whom have no relationship with the owner
beyond shared membership, none of whom were asked, and none of whom can tell
that a machine is now reading what they write. The owner can consent on their own
behalf. They cannot consent on behalf of the group.

That asymmetry is not fixed by infrastructure. The owner controlling the server,
the mailbox and the retention window improves the handling of the data; it does
not create permission to collect it. So the question this document has to answer
is not "can we forward groups safely" but "under what narrow conditions is
forwarding them defensible at all".

## What changes the calculus

Three things make a group different in kind, not just in degree:

1. **Scale of exposure.** One allow-listed group can put more third-party
   messages into the mailbox in a day than every one-to-one conversation
   combined.
2. **Blast radius of a reply.** A mistaken message to a contact is embarrassing.
   The same message to a work group is witnessed, forwarded and screenshotted.
   Every approval and quota rule needs to be stricter here, not equal.
3. **Attribution.** A group message has an author distinct from the chat.
   Forwarding without that distinction produces a mailbox where forty people
   speak in one voice, which is both useless to the assistant and a good way to
   attribute a sentence to the wrong person.

## Requirements

### Addressing

- **FR-G1 — Groups get their own address space.** A group has no phone number, so
  the `<number>@domain` convention does not apply. A group's local part is
  `g<opaque>`, derived as an HMAC of its JID, and never contains the group's
  name: a subject line is not the place to publish which groups someone is in.
- **FR-G2 — The address identifies the group, never the author.** A reply goes to
  the group, so the routable identity must be the group. Author identity travels
  in the display name and body, which are not routable.
- **FR-G3 — Group JIDs are accepted explicitly.** `phone.ParseJID` currently
  rejects `@g.us` outright. That rejection becomes conditional on group support
  being enabled, and stays the default.

### Forwarding

- **FR-G4 — Attribution is unambiguous.** Both the group and the author appear in
  the subject and again in the body:

  ```
  From:    "Marina · Projeto X" <g7f3a91c2@wa.example.com>
  Subject: [wa:…] Projeto X — Marina

  --- reply above this line ---

  Grupo:  Projeto X
  Autor:  Marina
  ```

  The assistant must never have to infer which of forty people wrote a line.
- **FR-G5 — Replies are visibly public.** A forward from a group carries a line
  saying a reply is seen by every member. The assistant renders this to the owner
  at approval time.
- **FR-G6 — System messages are dropped.** Joins, leaves, renames, icon changes
  and pinned-message notices are not conversation and are not forwarded.

### Permissions and limits

- **SEC-G1 — Groups are granted separately.** A group is never implied by any
  other grant. `actions` on a group entry is its own decision, and `send`
  requires the owner to have written it deliberately for that group.
- **SEC-G2 — No standing authorization.** `approve-except` does not apply to
  groups, in any mode, at any time. Every outbound group message is approved
  individually. This is the one place where the convenience is not worth it.
- **SEC-G3 — Separate, lower quotas.** Group sends are counted against their own
  cap, not the per-conversation cap, and the default is deliberately small.
- **SEC-G4 — Author allowlist, optional.** A group entry may name the members
  whose messages are forwarded. Everyone else's are dropped unread. This is the
  mechanism that makes "watch this group for messages from my manager" possible
  without reading the other thirty-nine people.
- **SEC-G5 — Shorter retention.** Group message bodies purge on a shorter window
  than one-to-one, by default. More people, less time.

### Operational

- **OPS-G1 — Volume is visible.** `status` reports group forwards separately, so
  a chatty group shows up as a number rather than as a surprising mail bill.
- **OPS-G2 — Decryption failures are expected.** Group messages use sender keys,
  and a message from a member whose key has not been received cannot be read.
  This is already observed in the log; it must be counted and not treated as an
  error condition.

## Open questions

1. **Does the owner tell the group?** Technically irrelevant, ethically central.
   A group where members know an assistant is reading is a different thing from
   one where they do not. Worth deciding deliberately rather than by default.
2. **Mentions.** Should a message that @mentions the owner bypass the author
   allowlist? It is the most useful case and the easiest to justify, and it is
   also a filter anyone in the group can trigger at will.
3. **Media.** Groups are where media actually lives. Still out of scope, but the
   gap is more conspicuous here than in one-to-one chats.
4. **Reply context.** A group reply usually quotes what it answers. Without the
   quoted context, an approved message can be correct and still land wrong.
5. **Leaving.** If the owner leaves a group, the entry should fail closed rather
   than sit in the config silently matching nothing.

## Suggested order

1. Read-only for one group, author allowlist required, no send path.
2. Attribution reviewed against real traffic: does the assistant reliably know
   who said what?
3. Volume and cost measured over a week.
4. Only then, sending — individually approved, never standing.
