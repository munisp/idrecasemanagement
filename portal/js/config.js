// Runtime config — overwritten per environment (nginx injects real values in prod).
window.IDRE_CONFIG = {
  keycloakUrl: "http://localhost:8085",   // https://auth.example.org in prod
  realm: "idre",
  clientId: "case-portal",
  apiBase: "",                            // same-origin; nginx proxies /v1 -> APISIX
  geoMapUrl: "",                          // GeoLibre saved-project URL (deploy/geolibre/README.md)
  demoMode: false,                        // true = run real portal code against built-in fixtures (js/demo.js), no backend
};
