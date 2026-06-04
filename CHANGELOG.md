# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Initial open source release of Pod NSG Controller.
- Kubernetes controller that manages Azure NSG rules via dynamic Application Security Groups (ASGs).
- Pod-level network security based on pod labels and annotations.
- Leader election support for high-availability deployments.
- Azure SDK for Go integration for ARM API interactions.
