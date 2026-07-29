use serde::{Deserialize, Serialize};

use crate::Error;

const WASM_PAGE_SIZE: u64 = 65_536;
const MIN_HOST_RESPONSE_SIZE: u32 = 256;

/// Engine-wide sandbox ceilings.
#[derive(Clone, Copy, Debug, Deserialize, PartialEq, Eq, Serialize)]
#[serde(default)]
pub struct EngineConfig {
    pub max_memory_bytes: u64,
    pub max_wasm_stack_bytes: u64,
    pub max_fuel: u64,
    pub max_source_bytes: u32,
    pub max_output_bytes: u32,
    pub max_stack: u32,
    pub max_imports: u32,
    pub max_import_bytes: u32,
    pub max_total_import_bytes: u64,
    pub max_capability_calls: u32,
    pub max_host_request_bytes: u32,
    pub max_host_response_bytes: u32,
    pub max_trace_bytes: u32,
    pub max_concurrent_evaluations: u32,
}

impl Default for EngineConfig {
    fn default() -> Self {
        Self {
            max_memory_bytes: 128 << 20,
            max_wasm_stack_bytes: 50_000_000,
            max_fuel: 100_000_000,
            max_source_bytes: 256 << 10,
            max_output_bytes: 1 << 20,
            max_stack: 256,
            max_imports: 64,
            max_import_bytes: 256 << 10,
            max_total_import_bytes: 2 << 20,
            max_capability_calls: 128,
            max_host_request_bytes: 512 << 10,
            max_host_response_bytes: 512 << 10,
            max_trace_bytes: 64 << 10,
            max_concurrent_evaluations: 4,
        }
    }
}

impl EngineConfig {
    pub const fn ceilings() -> Self {
        Self {
            max_memory_bytes: 1 << 30,
            max_wasm_stack_bytes: 256 << 20,
            max_fuel: 10_000_000_000,
            max_source_bytes: 16 << 20,
            max_output_bytes: 64 << 20,
            max_stack: 4096,
            max_imports: 4096,
            max_import_bytes: 16 << 20,
            max_total_import_bytes: 64 << 20,
            max_capability_calls: 10_000,
            max_host_request_bytes: 16 << 20,
            max_host_response_bytes: 32 << 20,
            max_trace_bytes: 4 << 20,
            max_concurrent_evaluations: 1024,
        }
    }

    pub fn validate(self) -> Result<Self, Error> {
        let ceiling = Self::ceilings();
        check(
            "max_memory_bytes",
            self.max_memory_bytes,
            ceiling.max_memory_bytes,
        )?;
        check(
            "max_wasm_stack_bytes",
            self.max_wasm_stack_bytes,
            ceiling.max_wasm_stack_bytes,
        )?;
        check("max_fuel", self.max_fuel, ceiling.max_fuel)?;
        check(
            "max_source_bytes",
            u64::from(self.max_source_bytes),
            u64::from(ceiling.max_source_bytes),
        )?;
        check(
            "max_output_bytes",
            u64::from(self.max_output_bytes),
            u64::from(ceiling.max_output_bytes),
        )?;
        check(
            "max_stack",
            u64::from(self.max_stack),
            u64::from(ceiling.max_stack),
        )?;
        check(
            "max_imports",
            u64::from(self.max_imports),
            u64::from(ceiling.max_imports),
        )?;
        check(
            "max_import_bytes",
            u64::from(self.max_import_bytes),
            u64::from(ceiling.max_import_bytes),
        )?;
        check(
            "max_total_import_bytes",
            self.max_total_import_bytes,
            ceiling.max_total_import_bytes,
        )?;
        check(
            "max_capability_calls",
            u64::from(self.max_capability_calls),
            u64::from(ceiling.max_capability_calls),
        )?;
        check(
            "max_host_request_bytes",
            u64::from(self.max_host_request_bytes),
            u64::from(ceiling.max_host_request_bytes),
        )?;
        check(
            "max_host_response_bytes",
            u64::from(self.max_host_response_bytes),
            u64::from(ceiling.max_host_response_bytes),
        )?;
        check(
            "max_trace_bytes",
            u64::from(self.max_trace_bytes),
            u64::from(ceiling.max_trace_bytes),
        )?;
        check(
            "max_concurrent_evaluations",
            u64::from(self.max_concurrent_evaluations),
            u64::from(ceiling.max_concurrent_evaluations),
        )?;
        if self.max_memory_bytes == 0 || !self.max_memory_bytes.is_multiple_of(WASM_PAGE_SIZE) {
            return Err(Error::invalid(
                "engine_config.max_memory_bytes",
                "must be a positive multiple of 65536",
            ));
        }
        if self.max_wasm_stack_bytes == 0
            || self.max_fuel == 0
            || self.max_source_bytes == 0
            || self.max_output_bytes == 0
            || self.max_stack == 0
            || self.max_imports == 0
            || self.max_import_bytes == 0
            || self.max_total_import_bytes == 0
            || self.max_capability_calls == 0
            || self.max_host_request_bytes == 0
            || self.max_trace_bytes == 0
            || self.max_concurrent_evaluations == 0
        {
            return Err(Error::invalid(
                "engine_config",
                "resource ceilings must be positive",
            ));
        }
        if self.max_host_response_bytes < MIN_HOST_RESPONSE_SIZE {
            return Err(Error::invalid(
                "engine_config.max_host_response_bytes",
                "must be at least 256",
            ));
        }
        Ok(self)
    }
}

fn check(field: &'static str, value: u64, ceiling: u64) -> Result<(), Error> {
    if value > ceiling {
        return Err(Error::invalid(
            field,
            format!("{value} exceeds hard ceiling {ceiling}"),
        ));
    }
    Ok(())
}

/// Per-evaluation overrides. Zero inherits the engine ceiling.
#[derive(Clone, Copy, Debug, Default, Deserialize, PartialEq, Eq, Serialize)]
#[serde(default)]
pub struct RequestLimits {
    pub max_fuel: u64,
    pub max_source_bytes: u32,
    pub max_output_bytes: u32,
    pub max_stack: u32,
    pub max_imports: u32,
    pub max_import_bytes: u32,
    pub max_total_import_bytes: u64,
    pub max_capability_calls: u32,
    pub max_host_request_bytes: u32,
    pub max_host_response_bytes: u32,
    pub max_trace_bytes: u32,
}

impl RequestLimits {
    pub(crate) fn resolve(self, ceiling: EngineConfig) -> Result<Self, Error> {
        macro_rules! inherit {
            ($field:ident) => {{
                if self.$field == 0 {
                    ceiling.$field
                } else if self.$field > ceiling.$field {
                    return Err(Error::invalid(
                        concat!("limits.", stringify!($field)),
                        format!("{} exceeds engine ceiling {}", self.$field, ceiling.$field),
                    ));
                } else {
                    self.$field
                }
            }};
        }
        let resolved = Self {
            max_fuel: inherit!(max_fuel),
            max_source_bytes: inherit!(max_source_bytes),
            max_output_bytes: inherit!(max_output_bytes),
            max_stack: inherit!(max_stack),
            max_imports: inherit!(max_imports),
            max_import_bytes: inherit!(max_import_bytes),
            max_total_import_bytes: inherit!(max_total_import_bytes),
            max_capability_calls: inherit!(max_capability_calls),
            max_host_request_bytes: inherit!(max_host_request_bytes),
            max_host_response_bytes: inherit!(max_host_response_bytes),
            max_trace_bytes: inherit!(max_trace_bytes),
        };
        if resolved.max_host_response_bytes < MIN_HOST_RESPONSE_SIZE {
            return Err(Error::invalid(
                "limits.max_host_response_bytes",
                "must be at least 256",
            ));
        }
        Ok(resolved)
    }
}
