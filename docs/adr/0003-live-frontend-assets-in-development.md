# Live frontend assets in development

Local development serves frontend assets directly from the working tree while Air continues to rebuild and restart the Go backend. Disable service-worker registration in development so refreshes load current files; automatic HMR is out of scope. This makes frontend iteration and testing immediate without changing the embedded-asset production path.
