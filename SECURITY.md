# Security Policy

## Supported Versions

Currently, only the latest version of Keystone receives security updates.

| Version | Supported |
|---------|-----------|
| Latest  | ✅        |
| Older   | ❌        |

## Reporting a Vulnerability

If you discover a security vulnerability, please report it responsibly.

### How to Report

**Do NOT** open a public issue for security vulnerabilities.

Instead, please send an email to: **security@archebase.com**

Include the following information in your report:

- **Description**: A clear description of the vulnerability
- **Impact**: The potential impact of the vulnerability
- **Steps to reproduce**: Detailed steps to reproduce the issue
- **Proof of concept**: If applicable, include a proof of concept
- **Affected versions**: Which versions are affected

### What to Expect

1. **Confirmation**: You will receive an email acknowledging receipt of your report
2. **Assessment**: We will assess the vulnerability and determine its severity
3. **Resolution**: We will work on a fix and coordinate disclosure with you
4. **Disclosure**: We will announce the security fix when a patch is available

### Response Time

We aim to respond to security reports within 48 hours and provide regular updates on our progress.

## Security Best Practices

When using Keystone with untrusted data:

1. **Validate Input**: Always validate data from untrusted sources
2. **Least Privilege**: Run services with the minimum required permissions
3. **Secrets Management**: Store credentials and tokens securely, never commit them to source control
4. **Resource Limits**: Set appropriate limits on request sizes and processing time
5. **Keep Updated**: Use the latest version to benefit from security fixes

## Security Features

Keystone includes several security-conscious design choices:

- **Input Validation**: Request validation and structured API handling
- **Service Separation**: Clear boundaries between API, storage, and background service components
- **Dependency Hygiene**: Go modules and automated dependency management help reduce risk

## Dependency Security

We regularly update dependencies to address security vulnerabilities:

- Automatic dependency updates via Dependabot or equivalent tooling
- Regular security reviews of dependencies
- Minimal dependency footprint where practical to reduce attack surface

## Disclosure Policy

We follow coordinated disclosure:

1. Fix the vulnerability
2. Release a new version
3. Publish a security advisory (if applicable)
4. Announce the fix

We do not disclose vulnerability details before a fix is available, unless the vulnerability is already publicly known.
