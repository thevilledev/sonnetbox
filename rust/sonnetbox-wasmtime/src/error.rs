use thiserror::Error;

/// A stable error taxonomy for callers of the Wasmtime host.
#[derive(Debug, Error)]
pub enum Error {
    #[error("invalid request field {field:?}: {message}")]
    InvalidRequest {
        field: &'static str,
        message: String,
    },
    #[error("import {path:?} from {from:?} denied: {message}")]
    ImportDenied {
        from: String,
        path: String,
        message: String,
    },
    #[error("import {path:?} from {from:?} failed: {message}")]
    Import {
        from: String,
        path: String,
        message: String,
    },
    #[error("capability {name:?} failed: {message}")]
    Capability { name: String, message: String },
    #[error("{resource} limit exceeded ({actual} > {limit})")]
    Limit {
        resource: &'static str,
        limit: u64,
        actual: u64,
    },
    #[error("evaluation canceled: deadline exceeded")]
    Canceled,
    #[error("guest trapped during {operation}: {source}")]
    GuestTrap {
        operation: &'static str,
        #[source]
        source: anyhow::Error,
    },
    #[error("jsonnet evaluation failed: {0}")]
    Evaluation(String),
    #[error("guest ABI error: {0}")]
    Abi(String),
}

impl Error {
    pub(crate) fn invalid(field: &'static str, message: impl Into<String>) -> Self {
        Self::InvalidRequest {
            field,
            message: message.into(),
        }
    }

    pub(crate) fn abi(message: impl Into<String>) -> Self {
        Self::Abi(message.into())
    }
}
