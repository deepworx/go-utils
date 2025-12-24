# Changelog

## [0.5.0](https://github.com/deepworx/go-utils/compare/v0.4.1...v0.5.0) (2025-12-24)


### Features

* **postgres:** add UnitOfWork pattern for testable transaction management ([1dab67f](https://github.com/deepworx/go-utils/commit/1dab67f7c26dc521c9c85d0aebb8b1fda0631012))


### Bug Fixes

* **jwtauth:** remove unused test helper functions ([3e9e604](https://github.com/deepworx/go-utils/commit/3e9e6043ee63bc73863b7aa8a9be9ca3ab0847e8))

## [0.4.1](https://github.com/deepworx/go-utils/compare/v0.4.0...v0.4.1) (2025-12-16)


### Bug Fixes

* **jwtauth:** add InferAlgorithmFromKey config option for JWKS without alg field ([8a6cada](https://github.com/deepworx/go-utils/commit/8a6cadafd8f95b2de7652c88d86f413c2b8c4576))

## [0.4.0](https://github.com/deepworx/go-utils/compare/v0.3.2...v0.4.0) (2025-12-16)


### Features

* **jwtauth:** add Validate() method to Config ([c97ad01](https://github.com/deepworx/go-utils/commit/c97ad0121543aa9421ebfc510899df4484effaed))

## [0.3.2](https://github.com/deepworx/go-utils/compare/v0.3.1...v0.3.2) (2025-12-15)


### Bug Fixes

* **interceptor:** disable server peer attributes to reduce metric cardinality ([e70dfb2](https://github.com/deepworx/go-utils/commit/e70dfb2c89cc4fde8313519064a416c0809e472d))

## [0.3.1](https://github.com/deepworx/go-utils/compare/v0.3.0...v0.3.1) (2025-12-15)


### Bug Fixes

* **jwtauth:** add context timeout to prevent hang on unreachable JWKS URL ([629fae4](https://github.com/deepworx/go-utils/commit/629fae45ea8e5183c6125919ec1b9e7f0d75d3c4))

## [0.3.0](https://github.com/deepworx/go-utils/compare/v0.2.1...v0.3.0) (2025-12-15)


### Features

* **grpchealth:** auto-start goroutine and register shutdown handler ([284d5ac](https://github.com/deepworx/go-utils/commit/284d5ac053d831ecea1382bf5f65e6d1253b57e3))
* **interceptor:** add default interceptor chain builder ([642cfea](https://github.com/deepworx/go-utils/commit/642cfea66739896351fdc37fdf1f6c1dd75b5d51))

## [0.2.1](https://github.com/deepworx/go-utils/compare/v0.2.0...v0.2.1) (2025-12-15)


### Bug Fixes

* **deps:** update go module dependencies ([750b66c](https://github.com/deepworx/go-utils/commit/750b66c20c3d59b27fbdfb6dce14ed96add51848))
