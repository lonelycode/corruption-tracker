# Security policy

## Reporting a vulnerability

Please report vulnerabilities **privately** through GitHub's
[private vulnerability reporting](https://docs.github.com/en/code-security/security-advisories/guidance-on-reporting-and-writing-information-about-vulnerabilities/privately-reporting-a-security-vulnerability)
on this repository: open the **Security** tab and choose **Report a
vulnerability**. Don't open a public issue or pull request for a security
problem.

Include what you found, how to reproduce it, and what an attacker could do
with it. Expect an acknowledgement within a week.

## What counts as a security issue here

Beyond the usual (authentication bypass, injection, privilege escalation,
secrets exposure), this project treats **any way the tracker could disclose
a name it should withhold** as a security issue. For example:

- a suppressed, Clean Slate-flagged or pre-verdict participant's name
  reaching a page, export, report or API response;
- a quarantined claim becoming visible or promotable without the required
  reviewers;
- a viewer reaching review, admin or export functions their role doesn't
  grant;
- report aggregates that let someone recover a cell smaller than the
  minimum cell size.

## Never include real data

Don't put real names, case details or exported data in a report, an issue,
a test fixture or a pull request. Use synthetic data to demonstrate a
problem. If a report has to reference a real record to be useful, describe
it without identifying anyone and say so; the maintainer will follow up
privately.

## Supported versions

Until the first tagged release, only the default branch is supported.
