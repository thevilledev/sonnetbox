use std::collections::BTreeMap;

use base64::Engine as _;
use serde::{Deserialize, Serialize};
use serde_json::Value;

use crate::{Error, OutputMode, RequestLimits};

pub(crate) const ABI_VERSION: u32 = 7;
pub(crate) const OP_RESOLVE_IMPORT: u32 = 1;
pub(crate) const OP_CALL_CAPABILITY: u32 = 2;

pub(crate) const HOST_OK: u32 = 0;
pub(crate) const HOST_DENIED: u32 = 1;
pub(crate) const HOST_HANDLER_FAILURE: u32 = 2;
pub(crate) const HOST_LIMIT: u32 = 3;
pub(crate) const HOST_CANCELED: u32 = 4;
pub(crate) const HOST_MALFORMED: u32 = 5;

pub(crate) const EVAL_OK: u32 = 0;
pub(crate) const EVAL_INVALID_REQUEST: u32 = 1;
pub(crate) const EVAL_JSONNET_ERROR: u32 = 2;
pub(crate) const EVAL_HOST_ERROR: u32 = 3;
pub(crate) const EVAL_LIMIT: u32 = 4;
pub(crate) const EVAL_INTERNAL: u32 = 5;

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Serialize)]
#[repr(u8)]
pub enum InputMode {
    #[default]
    Snippet = 0,
    File = 1,
    Anonymous = 2,
}

#[derive(Debug, Serialize)]
pub(crate) struct EvaluationRequest<'a> {
    pub filename: &'a str,
    pub source: &'a str,
    #[serde(skip_serializing_if = "is_zero")]
    pub input_mode: u8,
    #[serde(skip_serializing_if = "is_zero")]
    pub output_mode: u8,
    #[serde(skip_serializing_if = "BTreeMap::is_empty")]
    pub ext_vars: &'a BTreeMap<String, String>,
    #[serde(skip_serializing_if = "BTreeMap::is_empty")]
    pub ext_code: &'a BTreeMap<String, String>,
    #[serde(skip_serializing_if = "BTreeMap::is_empty")]
    pub tla_vars: &'a BTreeMap<String, String>,
    #[serde(skip_serializing_if = "BTreeMap::is_empty")]
    pub tla_code: &'a BTreeMap<String, String>,
    #[serde(skip_serializing_if = "BTreeMap::is_empty")]
    pub capabilities: BTreeMap<String, CapabilityDescriptor<'a>>,
    #[serde(skip_serializing_if = "is_false")]
    pub string_output: bool,
    pub output_newline: bool,
    #[serde(skip_serializing_if = "is_false")]
    pub capture_trace: bool,
    pub limits: GuestLimits,
}

#[derive(Debug, Serialize)]
pub(crate) struct CapabilityDescriptor<'a> {
    pub params: &'a [String],
}

#[derive(Clone, Copy, Debug, Serialize)]
pub(crate) struct GuestLimits {
    pub max_output_bytes: u32,
    pub max_stack: u32,
    pub max_host_request_bytes: u32,
    pub max_host_response_bytes: u32,
    pub max_trace_bytes: u32,
}

impl From<RequestLimits> for GuestLimits {
    fn from(value: RequestLimits) -> Self {
        Self {
            max_output_bytes: value.max_output_bytes,
            max_stack: value.max_stack,
            max_host_request_bytes: value.max_host_request_bytes,
            max_host_response_bytes: value.max_host_response_bytes,
            max_trace_bytes: value.max_trace_bytes,
        }
    }
}

#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct ImportRequest {
    #[serde(rename = "from")]
    pub imported_from: String,
    #[serde(rename = "path")]
    pub imported_path: String,
}

#[derive(Debug, Serialize)]
pub(crate) struct ImportResponse<'a> {
    pub canonical: &'a str,
    pub content_base64: String,
}

impl<'a> ImportResponse<'a> {
    pub fn new(canonical: &'a str, content: &[u8]) -> Self {
        Self {
            canonical,
            content_base64: base64::engine::general_purpose::STANDARD.encode(content),
        }
    }
}

#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct CapabilityRequest {
    pub name: String,
    pub args: Vec<Value>,
}

#[derive(Debug, Serialize)]
pub(crate) struct CapabilityResponse {
    pub value: Value,
}

#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct GuestError {
    pub kind: String,
    pub message: String,
    #[allow(dead_code)]
    pub limit: Option<u64>,
    #[allow(dead_code)]
    pub actual: Option<u64>,
}

pub(crate) fn pack(status: u32, length: usize) -> i64 {
    let packed = (u64::from(status) << 32) | length as u64;
    packed as i64
}

pub(crate) fn decode_output(mode: OutputMode, payload: &[u8]) -> Result<crate::Result, Error> {
    match mode {
        OutputMode::Single => Ok(crate::Result {
            output: payload.to_vec(),
            ..crate::Result::default()
        }),
        OutputMode::Multi => Ok(crate::Result {
            files: Some(decode_multi(payload)?),
            ..crate::Result::default()
        }),
        OutputMode::Stream => Ok(crate::Result {
            documents: Some(decode_stream(payload)?),
            ..crate::Result::default()
        }),
    }
}

fn decode_multi(mut input: &[u8]) -> Result<BTreeMap<String, Vec<u8>>, Error> {
    let count = take_u32(&mut input)?;
    let mut files = BTreeMap::new();
    for _ in 0..count {
        let name = String::from_utf8(take_bytes(&mut input)?.to_vec())
            .map_err(|error| Error::abi(format!("multi-file name is not UTF-8: {error}")))?;
        let output = take_bytes(&mut input)?.to_vec();
        if files.insert(name.clone(), output).is_some() {
            return Err(Error::abi(format!("duplicate multi-file output {name:?}")));
        }
    }
    if !input.is_empty() {
        return Err(Error::abi("trailing multi-file output bytes"));
    }
    Ok(files)
}

fn decode_stream(mut input: &[u8]) -> Result<Vec<Vec<u8>>, Error> {
    let count = take_u32(&mut input)?;
    let mut documents = Vec::with_capacity(count as usize);
    for _ in 0..count {
        documents.push(take_bytes(&mut input)?.to_vec());
    }
    if !input.is_empty() {
        return Err(Error::abi("trailing stream output bytes"));
    }
    Ok(documents)
}

fn take_u32(input: &mut &[u8]) -> Result<u32, Error> {
    let bytes = input
        .get(..4)
        .ok_or_else(|| Error::abi("truncated output frame"))?;
    *input = &input[4..];
    Ok(u32::from_le_bytes(bytes.try_into().expect("four bytes")))
}

fn take_bytes<'a>(input: &mut &'a [u8]) -> Result<&'a [u8], Error> {
    let length = take_u32(input)? as usize;
    let bytes = input
        .get(..length)
        .ok_or_else(|| Error::abi("output frame length exceeds payload"))?;
    *input = &input[length..];
    Ok(bytes)
}

const fn is_zero(value: &u8) -> bool {
    *value == 0
}

const fn is_false(value: &bool) -> bool {
    !*value
}
