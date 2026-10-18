# Security policy

Please report vulnerabilities privately through GitHub security advisories ("Report a vulnerability" on the repository's Security tab) rather than public issues. We aim to acknowledge reports within 5 working days.

Supported: the latest minor release.

Deployment notes:
- The CSP account needs Admin privilege on SCOS 7.2 direct REST. Use a dedicated account, keep its credentials in a Kubernetes Secret (`array.credentials.existingSecret`), and restrict the CSP Service with the chart's NetworkPolicy.
- Pin the array certificate (`array.tls.fingerprint` or `array.tls.caSecret`). `insecureSkipVerify` is for lab use only.
- Session tokens are sealed with the token key Secret; whoever can read it can forge tokens for the stored array credentials.
