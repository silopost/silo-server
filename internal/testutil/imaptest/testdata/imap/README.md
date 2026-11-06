# Dovecot test configuration

Used by `imaptest.Start`, which runs `dovecot/dovecot:2.4.5` (pinned) and copies
these files into the container before it starts:

| File | Container path | Purpose |
|---|---|---|
| `dovecot.conf` | `/etc/dovecot/dovecot.conf` | replaces the image config: IMAP only, no TLS, cleartext auth, maildir under `/tmp/home/%{user}` (lost with the container), special-use folders |
| `users` | `/etc/dovecot/users` | passwd-file with the test accounts `silo` / `silopass` and `silo2` / `silopass2` (a second mailbox: two Silo users, or a second account of one user) |
| `profile-<name>.conf` | `/etc/dovecot/profile.conf` | per-profile overrides (capabilities, folders, auth), included last; `@INTROSPECTION_URL@` is replaced by `StartOAuth2` |

Notes for Dovecot 2.4 (new settings syntax, `dovecot_config_version`):

- The image runs as the unprivileged user `vmail` and has no shell. The
  rootless defaults (internal users, listeners) come from
  `vendor.d/rootless.conf`, which `dovecot.conf` includes, so plaintext IMAP
  listens on **31143**, not 143.
- `imap_capability` replaces the whole list (it is not additive), so the
  `no-*` profiles spell out the Dovecot 2.4.5 post-login list minus the hidden
  extensions. It only changes what is advertised; a client that ignores
  CAPABILITY could still use the hidden commands.
- Cleartext auth needs both `ssl = no` and `auth_allow_cleartext = yes`.

Profiles (plan, section 2.1): `full` (Dovecot defaults), `no-condstore`
(no `CONDSTORE QRESYNC`), `no-move` (no `MOVE UIDPLUS`).

Phase 5 adds:

- `gmail-like`: Gmail's folders (`[Gmail]/All Mail` with `\All`,
  `[Gmail]/Sent Mail`, `[Gmail]/Drafts`, `[Gmail]/Trash`, `[Gmail]/Spam`; the
  default special folders lose their roles). Dovecot keeps one copy per
  folder: tests APPEND the same message to INBOX and All Mail to imitate a
  Gmail label. No `X-GM-EXT-1` (generic mode).
- `oauth2` (`imaptest.StartOAuth2`): `AUTH=XOAUTH2 OAUTHBEARER` through the
  `oauth2` passdb, which checks each token with RFC 7662 introspection at the
  fake provider of `testutil/oauthtest` on the host (`host.docker.internal`;
  on Linux mapped to the host gateway) and logs in the user its `email`
  names. Passwords still work for the passwd-file users.
