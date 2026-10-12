# Data ethics and operator responsibilities

This tracker records allegations and convictions about real people. Getting
that wrong can breach a court order, ruin a reputation or prejudice a trial.
This document sets out what the software enforces, what it deliberately
leaves to you as the operator, and where its data may come from.

It is not legal advice. If you're unsure whether something is lawful in your
situation, ask a lawyer before you publish anything derived from the
tracker.

## What the code enforces

These behaviours are built in. They aren't settings, and the project treats
any way around them as a security bug (see [SECURITY.md](SECURITY.md)).

- **Everything is reviewed.** Connectors only create *claims* in a review
  queue. A claim becomes a case only when a reviewer promotes it, and every
  review decision is recorded in an append-only audit trail that the
  database itself refuses to edit or delete.
- **Suppression quarantine.** A claim is quarantined automatically if the
  extractor reports a suppression mention, if the text matches the
  suppression signal list ("name suppression", "cannot be named", redaction
  markers and similar), or if an enforcement matter before verdict names
  nobody (the usual sign of interim suppression). Releasing a quarantined
  claim needs two different reviewers.
- **Naming rule.** A participant's name is shown only when naming is on
  for the tracker (see below) and *all* of these hold: the case hasn't been
  blocked from naming by a reviewer; no
  participant on the case carries a suppression or Clean Slate flag (one
  flag withholds every name on the case); and the case is past verdict, or
  it is pre-verdict but the prosecuting agency itself published the names in
  a media release. The same rule applies to case pages, digests, exports
  and reports.
- **ALLEGED.** Pre-verdict matters are always labelled as allegations with
  no findings made, and report aggregates never mix alleged and proven
  figures.
- **Small cells.** Report aggregates merge or drop any cell with fewer than
  three cases, so a report can't single out an individual.
- **Export is off by default.** Viewer accounts can't export until an admin
  grants it, and the grant is recorded with a reason.
- **No judgment-database automation.** The software never fetches from
  Judicial Decisions Online or NZLII. The appeal-check worklist opens search
  links in the reviewer's own browser, and the reviewer records the outcome
  by hand.

## What you are responsible for

- **Who gets access.** Only give accounts to people and organisations that
  have agreed in writing how they'll use the data, including that they'll
  verify suppression status and accuracy before publishing anything
  derived from it. Case pages, the explorer, reports and exports carry a
  notice to that effect, naming you as the operator.
- **Naming is off until you turn it on.** A new tracker shows nobody's
  name. Every case page, explorer view, export and report withholds names
  and the case summaries that carry them, and the explorer's search
  ignores names. An admin turns naming on under **Settings → Naming**. That
  needs a confirmation that everyone with access has agreed in writing to
  verify suppression status before publishing, and the tracker records who
  turned it on and when. Turn it on only when that is true.
- **Once naming is on**, a verified case shows names when it passes the
  naming rule above, and reviewers untick naming to block a specific case.
  Turning naming off again withholds every name at once.
- **Imported data.** An imported bundle keeps each case's own naming
  decision. The receiving tracker's naming still stays off until its own
  admin turns it on: the setting is never imported.
- **Reviewing properly.** Reviewers attest to what they promote. Check the
  source, check for suppression and Clean Slate eligibility (the Criminal
  Records (Clean Slate) Act 2004 conceals some old convictions), and check
  that the person named really is the person in the source.
- **Corrections.** When a matter changes (an appeal succeeds, charges are
  withdrawn, a suppression order is made), update the case promptly.
  Report snapshots are versioned: issue a corrected version with a revision
  note rather than editing history.
- **Privacy law.** The data is personal information under the Privacy Act
  2020. You are the agency holding it. Handle access requests, correction
  requests, retention and security accordingly.
- **Handing data over.** An export bundle contains everything, including
  unverified claims and audit trails. Only transfer one under a written
  data-transfer agreement that carries these obligations to the recipient.

## Where data may come from

| Source | Basis | How it is used |
|---|---|---|
| Agency media releases (SFO, IRD, Commerce Commission, MSD, Police, ACC, MBIE, DIA) | NZ Crown copyright, published under NZGOAL / CC BY 4.0. robots.txt is re-checked on every run, and a connector disables itself if its path is disallowed. | Extracted into claims; the licence basis is recorded on each claim. |
| News and commentary already held by the operator | Fair dealing for internal review only | Short excerpts stay internal and are never republished. |
| Alert emails sent to the operator's own mailbox (court notifications, licensed digests) | The operator's own subscriptions | Parsed into claims. |
| Manually researched records (for example a spreadsheet) | The operator's own research | Imported as unverified cases or claims for review. |

Judgment databases are not a source, and connectors for them must not be
added. If you add a connector, it must respect robots.txt, record its
licence basis, and route everything through the review queue.
