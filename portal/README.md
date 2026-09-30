# Federal IDRE Portal — PWA + native mobile

Framework-free, dependency-free PWA covering every backend surface (see
`docs/STAKEHOLDERS.md` §3 for the coverage matrix). The same codebase produces
installable PWA and native iOS/Android apps via Capacitor.

## Features

- **Auth**: Keycloak OIDC with PKCE (S256) — no secrets in the client, silent refresh.
- **PWA**: manifest + service worker; app shell works offline, API GETs fall back
  to cached data with an explicit `offline` indicator; installable on iOS/Android/desktop.
- **Role-aware UI**: nav and actions render from Keycloak realm roles
  (PARTY / ARBITRATOR / CASE_MANAGER / FEDERAL_ADMIN / STATE_AUDITOR / PLATFORM_ADMIN).
- **Screens**: Dashboard, Disputes, Case detail (workflow action buttons by role,
  sealed-offer submit, determination form), Documents (upload/download/analysis
  with extracted fields + findings), New dispute, Onboarding (self-service
  application + review queue with approve/reject), Voice console (intake + call logs),
  Compliance reports (status rollup + SLA breaches).

## Run (PWA)

Served by nginx in `docker-compose` at http://localhost:3001 (proxies `/v1` to APISIX).
For dev: `npm run serve`.

## Native builds (Capacitor)

```bash
cd portal
npm install
npx cap add ios && npx cap add android   # one-time
npx cap sync                              # copies web assets into the shells
npx cap open ios                          # Xcode -> archive for App Store
npx cap open android                      # Android Studio -> AAB for Play
```

Push notifications: `@capacitor/push-notifications` is wired in config; device
tokens register with the platform's notifier service (the same one Temporal
activities use for voice/email milestones).

## Icons

Replace `icons/icon.svg` with exported 192/512 PNGs (maskable) before store submission.
