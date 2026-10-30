use std::collections::BTreeMap;

pub const API_VERSION: u64 = 1;

pub const FLAG: &str = "--control-fd";

pub const MAX_MESSAGE: usize = 64 << 10;

pub mod method {
    pub const PAUSE: &str = "pause";
    pub const RESUME: &str = "resume";
    pub const STATS: &str = "stats";
    pub const STOP: &str = "stop";
    pub const KILL: &str = "kill";
    pub const BUDGET: &str = "budget";
    pub const RESET: &str = "reset";
    pub const SPACE: &str = "space";
}

pub mod code {
    pub const PARSE_ERROR: i64 = -32700;
    pub const INVALID_REQUEST: i64 = -32600;
    pub const METHOD_NOT_FOUND: i64 = -32601;
    pub const INVALID_PARAMS: i64 = -32602;
    pub const INTERNAL_ERROR: i64 = -32603;
    pub const FAILED: i64 = -32002;
    pub const STOPPING: i64 = -32006;
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum State {
    Running,
    Paused,
    Stopping,
}

impl State {
    pub fn name(self) -> &'static str {
        match self {
            State::Running => "running",
            State::Paused => "paused",
            State::Stopping => "stopping",
        }
    }

    fn named(name: &str) -> Result<State, String> {
        match name {
            "running" => Ok(State::Running),
            "paused" => Ok(State::Paused),
            "stopping" => Ok(State::Stopping),
            other => Err(format!("state {other:?}")),
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum WindowState {
    On,
    Off,
    Failed,
}

impl WindowState {
    pub fn name(self) -> &'static str {
        match self {
            WindowState::On => "on",
            WindowState::Off => "off",
            WindowState::Failed => "failed",
        }
    }

    fn named(name: &str) -> Result<WindowState, String> {
        match name {
            "on" => Ok(WindowState::On),
            "off" => Ok(WindowState::Off),
            "failed" => Ok(WindowState::Failed),
            other => Err(format!("window state {other:?}")),
        }
    }
}

pub trait Wire: Sized {
    fn encode(&self, out: &mut String);
    fn decode(v: &json::Value) -> Result<Self, String>;
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub struct Empty;

impl Wire for Empty {
    fn encode(&self, out: &mut String) {
        out.push_str("{}");
    }
    fn decode(v: &json::Value) -> Result<Self, String> {
        json::Fields::of(v)?.end()?;
        Ok(Empty)
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct PauseResult {
    pub state: State,
    pub changed: bool,
    pub vcpus: u64,
    pub took_ns: u64,
    pub paused_at_ns: u64,
}

impl Wire for PauseResult {
    fn encode(&self, out: &mut String) {
        let mut o = json::Object::begin(out);
        o.str("state", self.state.name());
        o.bool("changed", self.changed);
        o.num("vcpus", self.vcpus);
        o.num("took_ns", self.took_ns);
        o.num("paused_at_ns", self.paused_at_ns);
        o.end();
    }
    fn decode(v: &json::Value) -> Result<Self, String> {
        let mut f = json::Fields::of(v)?;
        let r = PauseResult {
            state: State::named(f.str("state")?)?,
            changed: f.bool("changed")?,
            vcpus: f.num("vcpus")?,
            took_ns: f.num("took_ns")?,
            paused_at_ns: f.num("paused_at_ns")?,
        };
        f.end()?;
        Ok(r)
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ResumeResult {
    pub state: State,
    pub changed: bool,
    pub vcpus: u64,
    pub took_ns: u64,
    pub offset_ns: u64,
    pub total_offset_ns: u64,
}

impl Wire for ResumeResult {
    fn encode(&self, out: &mut String) {
        let mut o = json::Object::begin(out);
        o.str("state", self.state.name());
        o.bool("changed", self.changed);
        o.num("vcpus", self.vcpus);
        o.num("took_ns", self.took_ns);
        o.num("offset_ns", self.offset_ns);
        o.num("total_offset_ns", self.total_offset_ns);
        o.end();
    }
    fn decode(v: &json::Value) -> Result<Self, String> {
        let mut f = json::Fields::of(v)?;
        let r = ResumeResult {
            state: State::named(f.str("state")?)?,
            changed: f.bool("changed")?,
            vcpus: f.num("vcpus")?,
            took_ns: f.num("took_ns")?,
            offset_ns: f.num("offset_ns")?,
            total_offset_ns: f.num("total_offset_ns")?,
        };
        f.end()?;
        Ok(r)
    }
}

#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct Vcpu {
    pub cpu_ns: u64,
    pub exits: u64,
}

impl Wire for Vcpu {
    fn encode(&self, out: &mut String) {
        let mut o = json::Object::begin(out);
        o.num("cpu_ns", self.cpu_ns);
        o.num("exits", self.exits);
        o.end();
    }
    fn decode(v: &json::Value) -> Result<Self, String> {
        let mut f = json::Fields::of(v)?;
        let r = Vcpu {
            cpu_ns: f.num("cpu_ns")?,
            exits: f.num("exits")?,
        };
        f.end()?;
        Ok(r)
    }
}

#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct Memory {
    pub guest_bytes: u64,
    pub footprint_bytes: u64,
    pub resident_bytes: u64,
    pub compressed_bytes: u64,
    pub released_bytes: u64,
    pub released_ranges: u64,
}

impl Wire for Memory {
    fn encode(&self, out: &mut String) {
        let mut o = json::Object::begin(out);
        o.num("guest_bytes", self.guest_bytes);
        o.num("footprint_bytes", self.footprint_bytes);
        o.num("resident_bytes", self.resident_bytes);
        o.num("compressed_bytes", self.compressed_bytes);
        o.num("released_bytes", self.released_bytes);
        o.num("released_ranges", self.released_ranges);
        o.end();
    }
    fn decode(v: &json::Value) -> Result<Self, String> {
        let mut f = json::Fields::of(v)?;
        let r = Memory {
            guest_bytes: f.num("guest_bytes")?,
            footprint_bytes: f.num("footprint_bytes")?,
            resident_bytes: f.num("resident_bytes")?,
            compressed_bytes: f.num("compressed_bytes")?,
            released_bytes: f.num("released_bytes")?,
            released_ranges: f.num("released_ranges")?,
        };
        f.end()?;
        Ok(r)
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Window {
    pub state: WindowState,
    pub reason: String,
    pub size_bytes: u64,
    pub guest_addr: u64,
    pub reads: u64,
    pub mappings: u64,
    pub unmappings: u64,
    pub live: u64,
    pub faults: u64,
}

impl Window {
    pub fn new(state: WindowState) -> Window {
        Window {
            state,
            reason: String::new(),
            size_bytes: 0,
            guest_addr: 0,
            reads: 0,
            mappings: 0,
            unmappings: 0,
            live: 0,
            faults: 0,
        }
    }
}

impl Wire for Window {
    fn encode(&self, out: &mut String) {
        let mut o = json::Object::begin(out);
        o.str("state", self.state.name());
        if !self.reason.is_empty() {
            o.str("reason", &self.reason);
        }
        o.num("size_bytes", self.size_bytes);
        for (name, n) in [
            ("guest_addr", self.guest_addr),
            ("reads", self.reads),
            ("mappings", self.mappings),
            ("unmappings", self.unmappings),
            ("live", self.live),
            ("faults", self.faults),
        ] {
            if n != 0 {
                o.num(name, n);
            }
        }
        o.end();
    }
    fn decode(v: &json::Value) -> Result<Self, String> {
        let mut f = json::Fields::of(v)?;
        let r = Window {
            state: WindowState::named(f.str("state")?)?,
            reason: f.str_or("reason")?.to_string(),
            size_bytes: f.num("size_bytes")?,
            guest_addr: f.num_or("guest_addr")?,
            reads: f.num_or("reads")?,
            mappings: f.num_or("mappings")?,
            unmappings: f.num_or("unmappings")?,
            live: f.num_or("live")?,
            faults: f.num_or("faults")?,
        };
        f.end()?;
        Ok(r)
    }
}

#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct Windows {
    pub data_disk: Option<Window>,
    pub file_share: Option<Window>,
}

impl Wire for Windows {
    fn encode(&self, out: &mut String) {
        let mut o = json::Object::begin(out);
        if let Some(w) = &self.data_disk {
            o.wire("data_disk", w);
        }
        if let Some(w) = &self.file_share {
            o.wire("file_share", w);
        }
        o.end();
    }
    fn decode(v: &json::Value) -> Result<Self, String> {
        let mut f = json::Fields::of(v)?;
        let r = Windows {
            data_disk: f.take("data_disk").map(Window::decode).transpose()?,
            file_share: f.take("file_share").map(Window::decode).transpose()?,
        };
        f.end()?;
        Ok(r)
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Stats {
    pub api_version: u64,
    pub build: String,
    pub state: State,
    pub at_ns: u64,
    pub pauses: u64,
    pub total_offset_ns: u64,
    pub paused_at_ns: u64,
    pub resets: u64,
    pub vcpus: Vec<Vcpu>,
    pub memory: Memory,
    pub devices: BTreeMap<String, BTreeMap<String, u64>>,
    pub windows: Option<Windows>,
}

impl Wire for Stats {
    fn encode(&self, out: &mut String) {
        let mut o = json::Object::begin(out);
        o.num("api_version", self.api_version);
        o.str("build", &self.build);
        o.str("state", self.state.name());
        o.num("at_ns", self.at_ns);
        o.num("pauses", self.pauses);
        o.num("total_offset_ns", self.total_offset_ns);
        if self.paused_at_ns != 0 {
            o.num("paused_at_ns", self.paused_at_ns);
        }
        o.num("resets", self.resets);
        o.list("vcpus", &self.vcpus);
        o.wire("memory", &self.memory);
        if !self.devices.is_empty() {
            let out = o.key("devices");
            let mut devices = json::Object::begin(out);
            for (device, counters) in &self.devices {
                let out = devices.key(device);
                let mut c = json::Object::begin(out);
                for (name, n) in counters {
                    c.num(name, *n);
                }
                c.end();
            }
            devices.end();
        }
        if let Some(w) = &self.windows {
            o.wire("windows", w);
        }
        o.end();
    }
    fn decode(v: &json::Value) -> Result<Self, String> {
        let mut f = json::Fields::of(v)?;
        let mut r = Stats {
            api_version: f.num("api_version")?,
            build: f.str("build")?.to_string(),
            state: State::named(f.str("state")?)?,
            at_ns: f.num("at_ns")?,
            pauses: f.num("pauses")?,
            total_offset_ns: f.num("total_offset_ns")?,
            paused_at_ns: f.num_or("paused_at_ns")?,
            resets: f.num("resets")?,
            vcpus: Vec::new(),
            memory: Memory::decode(f.need("memory")?)?,
            devices: BTreeMap::new(),
            windows: f.take("windows").map(Windows::decode).transpose()?,
        };
        for vcpu in f.need("vcpus")?.items()? {
            r.vcpus.push(Vcpu::decode(vcpu)?);
        }
        if let Some(devices) = f.take("devices") {
            for (device, counters) in devices.members()? {
                let mut named = BTreeMap::new();
                for (name, n) in counters.members()? {
                    named.insert(name.clone(), n.num()?);
                }
                r.devices.insert(device.clone(), named);
            }
        }
        f.end()?;
        Ok(r)
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Reset {
    pub count: u64,
    pub at_ns: u64,
}

impl Wire for Reset {
    fn encode(&self, out: &mut String) {
        let mut o = json::Object::begin(out);
        o.num("count", self.count);
        o.num("at_ns", self.at_ns);
        o.end();
    }
    fn decode(v: &json::Value) -> Result<Self, String> {
        let mut f = json::Fields::of(v)?;
        let r = Reset {
            count: f.num("count")?,
            at_ns: f.num("at_ns")?,
        };
        f.end()?;
        Ok(r)
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Baseline {
    pub available_bytes: u64,
    pub image_bytes: u64,
    pub margin_bytes: u64,
}

impl Wire for Baseline {
    fn encode(&self, out: &mut String) {
        let mut o = json::Object::begin(out);
        o.num("available_bytes", self.available_bytes);
        o.num("image_bytes", self.image_bytes);
        o.num("margin_bytes", self.margin_bytes);
        o.end();
    }
    fn decode(v: &json::Value) -> Result<Self, String> {
        let mut f = json::Fields::of(v)?;
        let r = Baseline {
            available_bytes: f.num("available_bytes")?,
            image_bytes: f.num("image_bytes")?,
            margin_bytes: f.num("margin_bytes")?,
        };
        f.end()?;
        Ok(r)
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct BudgetResult {
    pub held_requests: u64,
}

impl Wire for BudgetResult {
    fn encode(&self, out: &mut String) {
        let mut o = json::Object::begin(out);
        o.num("held_requests", self.held_requests);
        o.end();
    }
    fn decode(v: &json::Value) -> Result<Self, String> {
        let mut f = json::Fields::of(v)?;
        let r = BudgetResult {
            held_requests: f.num("held_requests")?,
        };
        f.end()?;
        Ok(r)
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Space {
    pub available_bytes: u64,
    pub image_bytes: u64,
    pub held_requests: u64,
    pub at_ns: u64,
}

impl Wire for Space {
    fn encode(&self, out: &mut String) {
        let mut o = json::Object::begin(out);
        o.num("available_bytes", self.available_bytes);
        o.num("image_bytes", self.image_bytes);
        o.num("held_requests", self.held_requests);
        o.num("at_ns", self.at_ns);
        o.end();
    }
    fn decode(v: &json::Value) -> Result<Self, String> {
        let mut f = json::Fields::of(v)?;
        let r = Space {
            available_bytes: f.num("available_bytes")?,
            image_bytes: f.num("image_bytes")?,
            held_requests: f.num("held_requests")?,
            at_ns: f.num("at_ns")?,
        };
        f.end()?;
        Ok(r)
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RpcError {
    pub code: i64,
    pub message: String,
}

impl RpcError {
    pub fn new(code: i64, message: impl Into<String>) -> RpcError {
        RpcError {
            code,
            message: message.into(),
        }
    }

    pub fn stopping() -> RpcError {
        RpcError::new(code::STOPPING, "the machine is stopping")
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Call {
    Pause,
    Resume,
    Stats,
    Stop,
    Kill,
    Budget(Baseline),
}

impl Call {
    pub fn method(self) -> &'static str {
        match self {
            Call::Pause => method::PAUSE,
            Call::Resume => method::RESUME,
            Call::Stats => method::STATS,
            Call::Stop => method::STOP,
            Call::Kill => method::KILL,
            Call::Budget(_) => method::BUDGET,
        }
    }
}

#[derive(Debug, Clone, PartialEq)]
pub struct Request {
    pub id: Option<String>,
    pub method: String,
    pub params: Option<json::Value>,
}

impl Request {
    pub fn call(&self) -> Result<Call, RpcError> {
        let call = match self.method.as_str() {
            method::BUDGET => {
                let params = self.params.as_ref().ok_or_else(|| {
                    RpcError::new(
                        code::INVALID_PARAMS,
                        "invalid parameters: budget takes available_bytes, image_bytes and margin_bytes",
                    )
                })?;
                return Baseline::decode(params).map(Call::Budget).map_err(|e| {
                    RpcError::new(code::INVALID_PARAMS, format!("invalid parameters: {e}"))
                });
            }
            method::PAUSE => Call::Pause,
            method::RESUME => Call::Resume,
            method::STATS => Call::Stats,
            method::STOP => Call::Stop,
            method::KILL => Call::Kill,
            other => {
                return Err(RpcError::new(
                    code::METHOD_NOT_FOUND,
                    format!("method {} not found", json::quoted(other)),
                ))
            }
        };
        match &self.params {
            None | Some(json::Value::Object(_)) => Ok(call),
            Some(_) => Err(RpcError::new(
                code::INVALID_PARAMS,
                format!("invalid parameters: {} takes none", self.method),
            )),
        }
    }
}

#[derive(Debug, Clone, PartialEq)]
pub enum Message {
    Request(Request),
    Reply {
        id: String,
        result: Result<json::Value, RpcError>,
    },
}

pub fn parse(line: &[u8]) -> Result<Message, RpcError> {
    let malformed = || RpcError::new(code::PARSE_ERROR, "malformed JSON-RPC message");
    if line.len() > MAX_MESSAGE {
        return Err(RpcError::new(
            code::PARSE_ERROR,
            format!("a message longer than {MAX_MESSAGE} bytes"),
        ));
    }
    let text = std::str::from_utf8(line).map_err(|_| malformed())?;
    let value = json::parse(text).map_err(|_| malformed())?;
    let mut f = json::Fields::of(&value).map_err(|_| malformed())?;
    match f.take("jsonrpc") {
        Some(json::Value::String(v)) if v == "2.0" => {}
        _ => return Err(malformed()),
    }
    let id = match f.take("id") {
        None => None,
        Some(id @ (json::Value::Number(_) | json::Value::String(_))) => Some(json::text(id)),
        Some(json::Value::Null) => Some("null".to_string()),
        Some(_) => {
            return Err(RpcError::new(
                code::INVALID_REQUEST,
                "an id is a number or a string",
            ))
        }
    };
    let method = match f.take("method") {
        None => None,
        Some(json::Value::String(m)) => Some(m.clone()),
        Some(_) => return Err(RpcError::new(code::INVALID_REQUEST, "a method is a string")),
    };
    let params = match f.take("params") {
        None | Some(json::Value::Null) => None,
        Some(v) => Some(v.clone()),
    };
    let result = f.take("result").cloned();
    let error = f.take("error");
    if let Some(method) = method {
        return Ok(Message::Request(Request { id, method, params }));
    }
    let Some(id) = id else {
        return Err(RpcError::new(
            code::INVALID_REQUEST,
            "neither a call nor a reply",
        ));
    };
    if let Some(error) = error {
        let mut e = json::Fields::of(error).map_err(|_| malformed())?;
        let code = e
            .need("code")
            .and_then(|c| c.int())
            .map_err(|_| malformed())?;
        let message = e.str("message").map_err(|_| malformed())?.to_string();
        return Ok(Message::Reply {
            id,
            result: Err(RpcError { code, message }),
        });
    }
    Ok(Message::Reply {
        id,
        result: Ok(result.unwrap_or(json::Value::Null)),
    })
}

pub fn reply(id: &str, result: &impl Wire) -> String {
    let mut out = format!("{{\"jsonrpc\":\"2.0\",\"id\":{id},\"result\":");
    result.encode(&mut out);
    out.push_str("}\n");
    out
}

pub fn refusal(id: Option<&str>, error: &RpcError) -> String {
    format!(
        "{{\"jsonrpc\":\"2.0\",\"id\":{},\"error\":{{\"code\":{},\"message\":{}}}}}\n",
        id.unwrap_or("null"),
        error.code,
        json::quoted(&error.message)
    )
}

pub fn notification(method: &str, params: &impl Wire) -> String {
    let mut out = format!(
        "{{\"jsonrpc\":\"2.0\",\"method\":{},\"params\":",
        json::quoted(method)
    );
    params.encode(&mut out);
    out.push_str("}\n");
    out
}

pub fn call(id: u64, call: Call) -> String {
    if let Call::Budget(b) = call {
        let mut out = format!(
            "{{\"jsonrpc\":\"2.0\",\"id\":{id},\"method\":\"{}\",\"params\":",
            call.method()
        );
        b.encode(&mut out);
        out.push_str("}\n");
        return out;
    }
    format!(
        "{{\"jsonrpc\":\"2.0\",\"id\":{id},\"method\":\"{}\"}}\n",
        call.method()
    )
}

pub mod json {
    use super::Wire;

    pub const MAX_DEPTH: usize = 16;

    #[derive(Debug, Clone, PartialEq)]
    pub enum Value {
        Null,
        Bool(bool),
        Number(String),
        String(String),
        Array(Vec<Value>),
        Object(Vec<(String, Value)>),
    }

    impl Value {
        pub fn num(&self) -> Result<u64, String> {
            match self {
                Value::Number(n) => n
                    .parse()
                    .map_err(|_| format!("{n} is not a non-negative integer")),
                other => Err(format!("{} where a number was expected", other.kind())),
            }
        }

        pub fn int(&self) -> Result<i64, String> {
            match self {
                Value::Number(n) => n.parse().map_err(|_| format!("{n} is not an integer")),
                other => Err(format!("{} where a number was expected", other.kind())),
            }
        }

        pub fn items(&self) -> Result<&[Value], String> {
            match self {
                Value::Array(items) => Ok(items),
                other => Err(format!("{} where a list was expected", other.kind())),
            }
        }

        pub fn members(&self) -> Result<&[(String, Value)], String> {
            match self {
                Value::Object(members) => Ok(members),
                other => Err(format!("{} where an object was expected", other.kind())),
            }
        }

        fn kind(&self) -> &'static str {
            match self {
                Value::Null => "null",
                Value::Bool(_) => "a boolean",
                Value::Number(_) => "a number",
                Value::String(_) => "a string",
                Value::Array(_) => "a list",
                Value::Object(_) => "an object",
            }
        }

        pub fn same(&self, other: &Value) -> bool {
            match (self, other) {
                (Value::Number(a), Value::Number(b)) => {
                    a == b || a.parse::<f64>().ok() == b.parse::<f64>().ok()
                }
                (Value::Array(a), Value::Array(b)) => {
                    a.len() == b.len() && a.iter().zip(b).all(|(x, y)| x.same(y))
                }
                (Value::Object(a), Value::Object(b)) => {
                    a.len() == b.len()
                        && a.iter()
                            .all(|(k, x)| b.iter().any(|(k2, y)| k == k2 && x.same(y)))
                }
                (a, b) => a == b,
            }
        }
    }

    pub struct Fields<'a> {
        members: &'a [(String, Value)],
        taken: Vec<bool>,
    }

    impl<'a> Fields<'a> {
        pub fn of(v: &'a Value) -> Result<Fields<'a>, String> {
            let members = v.members()?;
            for (i, (name, _)) in members.iter().enumerate() {
                if members[..i].iter().any(|(earlier, _)| earlier == name) {
                    return Err(format!("field {name:?} twice"));
                }
            }
            Ok(Fields {
                members,
                taken: vec![false; members.len()],
            })
        }

        pub fn take(&mut self, name: &str) -> Option<&'a Value> {
            let i = self.members.iter().position(|(n, _)| n == name)?;
            self.taken[i] = true;
            Some(&self.members[i].1)
        }

        pub fn need(&mut self, name: &str) -> Result<&'a Value, String> {
            self.take(name).ok_or_else(|| format!("no field {name:?}"))
        }

        pub fn num(&mut self, name: &str) -> Result<u64, String> {
            self.need(name)?.num().map_err(|e| format!("{name}: {e}"))
        }

        pub fn num_or(&mut self, name: &str) -> Result<u64, String> {
            match self.take(name) {
                Some(v) => v.num().map_err(|e| format!("{name}: {e}")),
                None => Ok(0),
            }
        }

        pub fn bool(&mut self, name: &str) -> Result<bool, String> {
            match self.need(name)? {
                Value::Bool(b) => Ok(*b),
                other => Err(format!(
                    "{name}: {} where a boolean was expected",
                    other.kind()
                )),
            }
        }

        pub fn str(&mut self, name: &str) -> Result<&'a str, String> {
            match self.need(name)? {
                Value::String(s) => Ok(s),
                other => Err(format!(
                    "{name}: {} where a string was expected",
                    other.kind()
                )),
            }
        }

        pub fn str_or(&mut self, name: &str) -> Result<&'a str, String> {
            match self.take(name) {
                Some(Value::String(s)) => Ok(s),
                Some(other) => Err(format!(
                    "{name}: {} where a string was expected",
                    other.kind()
                )),
                None => Ok(""),
            }
        }

        pub fn end(self) -> Result<(), String> {
            match self.taken.iter().position(|taken| !taken) {
                Some(i) => Err(format!("unknown field {:?}", self.members[i].0)),
                None => Ok(()),
            }
        }
    }

    pub struct Object<'a> {
        out: &'a mut String,
        first: bool,
    }

    impl<'a> Object<'a> {
        pub fn begin(out: &'a mut String) -> Object<'a> {
            out.push('{');
            Object { out, first: true }
        }

        pub fn key(&mut self, name: &str) -> &mut String {
            if !self.first {
                self.out.push(',');
            }
            self.first = false;
            self.out.push_str(&quoted(name));
            self.out.push(':');
            self.out
        }

        pub fn num(&mut self, name: &str, n: u64) {
            let out = self.key(name);
            out.push_str(&n.to_string());
        }

        pub fn bool(&mut self, name: &str, b: bool) {
            self.key(name).push_str(if b { "true" } else { "false" });
        }

        pub fn str(&mut self, name: &str, s: &str) {
            let out = self.key(name);
            out.push_str(&quoted(s));
        }

        pub fn wire(&mut self, name: &str, v: &impl Wire) {
            v.encode(self.key(name));
        }

        pub fn list(&mut self, name: &str, items: &[impl Wire]) {
            let out = self.key(name);
            out.push('[');
            for (i, item) in items.iter().enumerate() {
                if i > 0 {
                    out.push(',');
                }
                item.encode(out);
            }
            out.push(']');
        }

        pub fn end(self) {
            self.out.push('}');
        }
    }

    pub fn quoted(s: &str) -> String {
        let mut out = String::with_capacity(s.len() + 2);
        out.push('"');
        for c in s.chars() {
            match c {
                '"' => out.push_str("\\\""),
                '\\' => out.push_str("\\\\"),
                '\n' => out.push_str("\\n"),
                '\r' => out.push_str("\\r"),
                '\t' => out.push_str("\\t"),
                c if (c as u32) < 0x20 => out.push_str(&format!("\\u{:04x}", c as u32)),
                c => out.push(c),
            }
        }
        out.push('"');
        out
    }

    pub fn text(v: &Value) -> String {
        let mut out = String::new();
        write(v, &mut out);
        out
    }

    fn write(v: &Value, out: &mut String) {
        match v {
            Value::Null => out.push_str("null"),
            Value::Bool(b) => out.push_str(if *b { "true" } else { "false" }),
            Value::Number(n) => out.push_str(n),
            Value::String(s) => out.push_str(&quoted(s)),
            Value::Array(items) => {
                out.push('[');
                for (i, item) in items.iter().enumerate() {
                    if i > 0 {
                        out.push(',');
                    }
                    write(item, out);
                }
                out.push(']');
            }
            Value::Object(members) => {
                out.push('{');
                for (i, (name, value)) in members.iter().enumerate() {
                    if i > 0 {
                        out.push(',');
                    }
                    out.push_str(&quoted(name));
                    out.push(':');
                    write(value, out);
                }
                out.push('}');
            }
        }
    }

    pub fn parse(text: &str) -> Result<Value, String> {
        let mut p = Parser {
            bytes: text.as_bytes(),
            at: 0,
        };
        p.space();
        let v = p.value(0)?;
        p.space();
        if p.at != p.bytes.len() {
            return Err(format!("something after the value at byte {}", p.at));
        }
        Ok(v)
    }

    struct Parser<'a> {
        bytes: &'a [u8],
        at: usize,
    }

    impl Parser<'_> {
        fn peek(&self) -> Option<u8> {
            self.bytes.get(self.at).copied()
        }

        fn space(&mut self) {
            while matches!(self.peek(), Some(b' ' | b'\t' | b'\n' | b'\r')) {
                self.at += 1;
            }
        }

        fn expect(&mut self, b: u8) -> Result<(), String> {
            if self.peek() == Some(b) {
                self.at += 1;
                Ok(())
            } else {
                Err(format!("expected {:?} at byte {}", b as char, self.at))
            }
        }

        fn word(&mut self, word: &str, v: Value) -> Result<Value, String> {
            if self.bytes[self.at..].starts_with(word.as_bytes()) {
                self.at += word.len();
                Ok(v)
            } else {
                Err(format!("not a value at byte {}", self.at))
            }
        }

        fn value(&mut self, depth: usize) -> Result<Value, String> {
            if depth > MAX_DEPTH {
                return Err(format!("nested deeper than {MAX_DEPTH}"));
            }
            match self.peek() {
                Some(b'n') => self.word("null", Value::Null),
                Some(b't') => self.word("true", Value::Bool(true)),
                Some(b'f') => self.word("false", Value::Bool(false)),
                Some(b'"') => Ok(Value::String(self.string()?)),
                Some(b'-' | b'0'..=b'9') => self.number(),
                Some(b'[') => {
                    self.at += 1;
                    let mut items = Vec::new();
                    self.space();
                    if self.peek() == Some(b']') {
                        self.at += 1;
                        return Ok(Value::Array(items));
                    }
                    loop {
                        self.space();
                        items.push(self.value(depth + 1)?);
                        self.space();
                        match self.peek() {
                            Some(b',') => self.at += 1,
                            Some(b']') => {
                                self.at += 1;
                                return Ok(Value::Array(items));
                            }
                            _ => return Err(format!("a list is not closed at byte {}", self.at)),
                        }
                    }
                }
                Some(b'{') => {
                    self.at += 1;
                    let mut members = Vec::new();
                    self.space();
                    if self.peek() == Some(b'}') {
                        self.at += 1;
                        return Ok(Value::Object(members));
                    }
                    loop {
                        self.space();
                        let name = self.string()?;
                        self.space();
                        self.expect(b':')?;
                        self.space();
                        members.push((name, self.value(depth + 1)?));
                        self.space();
                        match self.peek() {
                            Some(b',') => self.at += 1,
                            Some(b'}') => {
                                self.at += 1;
                                return Ok(Value::Object(members));
                            }
                            _ => {
                                return Err(format!("an object is not closed at byte {}", self.at))
                            }
                        }
                    }
                }
                _ => Err(format!("not a value at byte {}", self.at)),
            }
        }

        fn digits(&mut self) -> usize {
            let start = self.at;
            while matches!(self.peek(), Some(b'0'..=b'9')) {
                self.at += 1;
            }
            self.at - start
        }

        fn number(&mut self) -> Result<Value, String> {
            let start = self.at;
            if self.peek() == Some(b'-') {
                self.at += 1;
            }
            let whole = self.at;
            if self.digits() == 0 || (self.bytes[whole] == b'0' && self.at - whole > 1) {
                return Err(format!("not a number at byte {start}"));
            }
            if self.peek() == Some(b'.') {
                self.at += 1;
                if self.digits() == 0 {
                    return Err(format!("not a number at byte {start}"));
                }
            }
            if matches!(self.peek(), Some(b'e' | b'E')) {
                self.at += 1;
                if matches!(self.peek(), Some(b'+' | b'-')) {
                    self.at += 1;
                }
                if self.digits() == 0 {
                    return Err(format!("not a number at byte {start}"));
                }
            }
            Ok(Value::Number(
                String::from_utf8_lossy(&self.bytes[start..self.at]).into_owned(),
            ))
        }

        fn hex4(&mut self) -> Result<u32, String> {
            let digits = self
                .bytes
                .get(self.at..self.at + 4)
                .and_then(|d| std::str::from_utf8(d).ok())
                .filter(|d| d.bytes().all(|b| b.is_ascii_hexdigit()))
                .ok_or_else(|| format!("a bad \\u escape at byte {}", self.at))?;
            self.at += 4;
            Ok(u32::from_str_radix(digits, 16).unwrap_or(0))
        }

        fn string(&mut self) -> Result<String, String> {
            self.expect(b'"')?;
            let mut out = Vec::new();
            loop {
                let Some(b) = self.peek() else {
                    return Err("a string is not closed".to_string());
                };
                self.at += 1;
                match b {
                    b'"' => break,
                    b'\\' => {
                        let Some(e) = self.peek() else {
                            return Err("a string is not closed".to_string());
                        };
                        self.at += 1;
                        let c = match e {
                            b'"' => '"',
                            b'\\' => '\\',
                            b'/' => '/',
                            b'b' => '\u{8}',
                            b'f' => '\u{c}',
                            b'n' => '\n',
                            b'r' => '\r',
                            b't' => '\t',
                            b'u' => {
                                let mut unit = self.hex4()?;
                                if (0xd800..0xdc00).contains(&unit) {
                                    if self.bytes[self.at..].starts_with(b"\\u") {
                                        self.at += 2;
                                        let low = self.hex4()?;
                                        if !(0xdc00..0xe000).contains(&low) {
                                            return Err("half of a surrogate pair".to_string());
                                        }
                                        unit = 0x10000 + ((unit - 0xd800) << 10) + (low - 0xdc00);
                                    } else {
                                        return Err("half of a surrogate pair".to_string());
                                    }
                                }
                                char::from_u32(unit).ok_or("half of a surrogate pair")?
                            }
                            _ => return Err(format!("a bad escape at byte {}", self.at - 1)),
                        };
                        out.extend_from_slice(c.encode_utf8(&mut [0; 4]).as_bytes());
                    }
                    b if b < 0x20 => {
                        return Err(format!(
                            "a control byte in a string at byte {}",
                            self.at - 1
                        ))
                    }
                    b => out.push(b),
                }
            }
            String::from_utf8(out).map_err(|_| "a string that is not UTF-8".to_string())
        }
    }
}

#[cfg(test)]
mod tests {
    use super::json::Value;
    use super::*;
    use std::collections::BTreeMap;
    use std::path::PathBuf;

    fn fixtures_dir() -> PathBuf {
        PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../../proto/vmmctl/fixtures")
    }

    fn fixtures() -> Vec<(String, String, bool, Value, Option<Value>)> {
        let mut out = Vec::new();
        for entry in std::fs::read_dir(fixtures_dir()).expect("proto/vmmctl/fixtures") {
            let path = entry.unwrap().path();
            if path.extension().and_then(|e| e.to_str()) != Some("json") {
                continue;
            }
            let name = path.file_name().unwrap().to_string_lossy().into_owned();
            let text = std::fs::read_to_string(&path).unwrap();
            let v = json::parse(&text).unwrap_or_else(|e| panic!("{name}: {e}"));
            let mut f = json::Fields::of(&v).unwrap();
            let method = f.str("method").unwrap().to_string();
            let notification = matches!(f.take("notification"), Some(Value::Bool(true)));
            let params = f.need("params").unwrap().clone();
            let result = f.take("result").cloned();
            f.end().unwrap_or_else(|e| panic!("{name}: {e}"));
            assert!(
                name.starts_with(&format!("{method}.")),
                "{name} holds {method}"
            );
            out.push((name, method, notification, params, result));
        }
        out.sort_by(|a, b| a.0.cmp(&b.0));
        out
    }

    fn again(method: &str, v: &Value) -> Result<String, String> {
        fn round<T: Wire>(v: &Value) -> Result<String, String> {
            let mut out = String::new();
            T::decode(v)?.encode(&mut out);
            Ok(out)
        }
        match method {
            method::PAUSE => round::<PauseResult>(v),
            method::RESUME => round::<ResumeResult>(v),
            method::STATS => round::<Stats>(v),
            method::STOP | method::KILL => round::<Empty>(v),
            method::RESET => round::<Reset>(v),
            method::BUDGET => round::<BudgetResult>(v),
            method::SPACE => round::<Space>(v),
            other => Err(format!("no type for {other}")),
        }
    }

    #[test]
    fn every_fixture_decodes_with_nothing_left_over_and_encodes_back() {
        let all = fixtures();
        assert!(all.len() >= 11, "only {} fixtures found", all.len());
        let mut seen = BTreeMap::new();
        for (name, method, notification, params, result) in &all {
            *seen.entry(method.as_str()).or_insert(0) += 1;
            assert_eq!(
                *notification,
                method == method::RESET || method == method::SPACE,
                "{name}"
            );
            let body = if *notification {
                assert!(result.is_none(), "{name}: a notification with a result");
                params
            } else {
                if method == method::BUDGET {
                    let mut out = String::new();
                    Baseline::decode(params)
                        .unwrap_or_else(|e| panic!("{name}: {e}"))
                        .encode(&mut out);
                    assert!(json::parse(&out).unwrap().same(params), "{name}: {out}");
                } else {
                    assert!(Empty::decode(params).is_ok(), "{name}: params {params:?}");
                }
                result
                    .as_ref()
                    .unwrap_or_else(|| panic!("{name}: no result"))
            };
            let text = again(method, body).unwrap_or_else(|e| panic!("{name}: {e}"));
            let back = json::parse(&text).unwrap_or_else(|e| panic!("{name}: wrote {text}: {e}"));
            assert!(
                back.same(body),
                "{name}: wrote {text}, the fixture says {body:?}"
            );
        }
        for method in [
            method::PAUSE,
            method::RESUME,
            method::STATS,
            method::STOP,
            method::KILL,
            method::RESET,
            method::BUDGET,
            method::SPACE,
        ] {
            assert!(seen.contains_key(method), "no fixture for {method}");
        }
        assert_eq!(
            seen.len(),
            8,
            "a fixture for a method this file does not have: {seen:?}"
        );
    }

    fn session() -> Vec<(bool, String)> {
        let text = std::fs::read_to_string(fixtures_dir().join("wire/session.jsonl")).unwrap();
        text.lines()
            .map(|line| match line.split_at(2) {
                ("> ", rest) => (true, rest.to_string()),
                ("< ", rest) => (false, rest.to_string()),
                _ => panic!("a line of the transcript names no side: {line}"),
            })
            .collect()
    }

    fn same_line(ours: &str, theirs: &str) {
        assert!(ours.ends_with('\n'), "{ours:?} does not end its line");
        let a = json::parse(ours.trim_end()).unwrap_or_else(|e| panic!("we wrote {ours}: {e}"));
        let b = json::parse(theirs).unwrap();
        assert!(
            a.same(&b),
            "we write\n {ours} the transcript has\n {theirs}"
        );
    }

    #[test]
    fn the_wire_session_is_read_and_written_line_for_line() {
        let lines = session();
        assert!(lines.len() >= 20, "only {} lines", lines.len());
        let mut asked: BTreeMap<String, Request> = BTreeMap::new();
        let (mut calls, mut refused, mut notes, mut unchanged, mut spaces) = (0, 0, 0, 0, 0);
        for (from_daemon, line) in &lines {
            let message = parse(line.as_bytes()).unwrap_or_else(|e| panic!("{line}: {e:?}"));
            match (from_daemon, message) {
                (true, Message::Request(req)) => {
                    let id = req.id.clone().expect("the daemon sends no notification");
                    if let Ok(call) = req.call() {
                        same_line(&super::call(id.parse().unwrap(), call), line);
                    }
                    asked.insert(id, req);
                    calls += 1;
                }
                (false, Message::Reply { id, result }) => {
                    let req = asked
                        .remove(&id)
                        .unwrap_or_else(|| panic!("a reply to {id}, which nobody asked"));
                    match (req.call(), result) {
                        (Ok(call), Ok(value)) => {
                            let ours = match call {
                                Call::Pause => {
                                    let r = PauseResult::decode(&value).unwrap();
                                    assert_eq!(r.state, State::Paused);
                                    unchanged += !r.changed as u32;
                                    reply(&id, &r)
                                }
                                Call::Resume => {
                                    let r = ResumeResult::decode(&value).unwrap();
                                    assert_eq!(r.state, State::Running);
                                    assert_eq!(r.changed, r.offset_ns != 0, "{line}");
                                    unchanged += !r.changed as u32;
                                    reply(&id, &r)
                                }
                                Call::Stats => reply(&id, &Stats::decode(&value).unwrap()),
                                Call::Stop | Call::Kill => {
                                    reply(&id, &Empty::decode(&value).unwrap())
                                }
                                Call::Budget(_) => {
                                    reply(&id, &BudgetResult::decode(&value).unwrap())
                                }
                            };
                            same_line(&ours, line);
                        }
                        (Err(ours), Err(theirs)) => {
                            assert_eq!(ours, theirs, "{line}");
                            same_line(&refusal(Some(&id), &ours), line);
                            refused += 1;
                        }
                        (Ok(_), Err(theirs)) => {
                            assert_eq!(theirs, RpcError::stopping(), "{line}");
                            same_line(&refusal(Some(&id), &theirs), line);
                            refused += 1;
                        }
                        (Err(e), Ok(_)) => panic!("{line}: a result for a call we refuse: {e:?}"),
                    }
                }
                (false, Message::Request(req)) => {
                    assert_eq!(req.id, None, "{line}");
                    if req.method == method::SPACE {
                        let space = Space::decode(&req.params.unwrap()).unwrap();
                        same_line(&notification(method::SPACE, &space), line);
                        spaces += 1;
                        continue;
                    }
                    assert_eq!(req.method.as_str(), method::RESET, "{line}");
                    let reset = Reset::decode(&req.params.unwrap()).unwrap();
                    same_line(&notification(method::RESET, &reset), line);
                    notes += 1;
                }
                (true, Message::Reply { .. }) => panic!("the daemon answers nothing: {line}"),
            }
        }
        assert!(asked.is_empty(), "calls with no answer: {asked:?}");
        assert!(calls >= 13 && refused == 2 && notes == 1 && unchanged == 2 && spaces == 2,
            "{calls} calls, {refused} refused, {notes} resets, {spaces} space notifications, {unchanged} that changed nothing");
    }

    #[test]
    fn a_call_is_told_from_what_is_not_one() {
        let request = |line: &str| match parse(line.as_bytes()) {
            Ok(Message::Request(r)) => r,
            other => panic!("{line}: {other:?}"),
        };
        for line in [
            r#"{"jsonrpc":"2.0","id":1,"method":"pause"}"#,
            r#"{"jsonrpc":"2.0","id":1,"method":"pause","params":null}"#,
            r#"{"jsonrpc":"2.0","id":1,"method":"pause","params":{}}"#,
            r#" { "method" : "pause", "id" : 1, "jsonrpc" : "2.0" } "#,
        ] {
            let r = request(line);
            assert_eq!(
                (r.id.as_deref(), r.call()),
                (Some("1"), Ok(Call::Pause)),
                "{line}"
            );
        }
        for (call, name) in [
            (Call::Pause, "pause"),
            (Call::Resume, "resume"),
            (Call::Stats, "stats"),
            (Call::Stop, "stop"),
            (Call::Kill, "kill"),
        ] {
            assert_eq!(call.method(), name);
            assert_eq!(request(&super::call(7, call)).call(), Ok(call));
        }
        let r = request(r#"{"jsonrpc":"2.0","id":"a\"b","method":"stats"}"#);
        assert_eq!(r.id.as_deref(), Some(r#""a\"b""#));
        assert_eq!(
            refusal(r.id.as_deref(), &RpcError::stopping()),
            "{\"jsonrpc\":\"2.0\",\"id\":\"a\\\"b\",\"error\":{\"code\":-32006,\"message\":\"the machine is stopping\"}}\n"
        );
        assert_eq!(request(r#"{"jsonrpc":"2.0","method":"stats"}"#).id, None);

        let code_of = |line: &str| request(line).call().unwrap_err().code;
        assert_eq!(
            code_of(r#"{"jsonrpc":"2.0","id":1,"method":"reset"}"#),
            code::METHOD_NOT_FOUND
        );
        assert_eq!(
            code_of(r#"{"jsonrpc":"2.0","id":1,"method":"Pause"}"#),
            code::METHOD_NOT_FOUND
        );
        assert_eq!(
            code_of(r#"{"jsonrpc":"2.0","id":1,"method":"pause","params":[1]}"#),
            code::INVALID_PARAMS
        );
        assert_eq!(
            code_of(r#"{"jsonrpc":"2.0","id":1,"method":"pause","params":"now"}"#),
            code::INVALID_PARAMS
        );

        let refused = |line: &[u8]| parse(line).unwrap_err().code;
        assert_eq!(refused(b""), code::PARSE_ERROR);
        assert_eq!(refused(b"pause"), code::PARSE_ERROR);
        assert_eq!(refused(b"[1,2]"), code::PARSE_ERROR);
        assert_eq!(refused(br#"{"id":1,"method":"pause"}"#), code::PARSE_ERROR);
        assert_eq!(
            refused(br#"{"jsonrpc":"1.0","id":1,"method":"pause"}"#),
            code::PARSE_ERROR
        );
        assert_eq!(
            refused(br#"{"jsonrpc":"2.0","id":1,"method":"pause"} x"#),
            code::PARSE_ERROR
        );
        assert_eq!(
            refused(br#"{"jsonrpc":"2.0","id":1,"id":2,"method":"pause"}"#),
            code::PARSE_ERROR
        );
        assert_eq!(
            refused(b"{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"pa\xffuse\"}"),
            code::PARSE_ERROR
        );
        assert_eq!(
            refused(br#"{"jsonrpc":"2.0","id":[1],"method":"pause"}"#),
            code::INVALID_REQUEST
        );
        assert_eq!(
            refused(br#"{"jsonrpc":"2.0","id":1,"method":7}"#),
            code::INVALID_REQUEST
        );
        assert_eq!(refused(br#"{"jsonrpc":"2.0"}"#), code::INVALID_REQUEST);
        let long = format!(
            r#"{{"jsonrpc":"2.0","id":1,"method":"{}"}}"#,
            "p".repeat(MAX_MESSAGE)
        );
        assert_eq!(refused(long.as_bytes()), code::PARSE_ERROR);
        let deep = format!(
            r#"{{"jsonrpc":"2.0","id":1,"method":"pause","params":{}{}}}"#,
            "[".repeat(40),
            "]".repeat(40)
        );
        assert_eq!(refused(deep.as_bytes()), code::PARSE_ERROR);
    }

    #[test]
    fn numbers_stay_exact_and_strings_survive() {
        let r = Reset {
            count: u64::MAX,
            at_ns: (1 << 53) + 1,
        };
        let line = notification(method::RESET, &r);
        let Ok(Message::Request(req)) = parse(line.trim_end().as_bytes()) else {
            panic!("{line}")
        };
        assert_eq!(Reset::decode(&req.params.unwrap()).unwrap(), r);
        for bad in ["-1", "1.5", "1e3", "18446744073709551616", "\"1\"", "null"] {
            let v = json::parse(&format!(r#"{{"count":{bad},"at_ns":1}}"#)).unwrap();
            assert!(Reset::decode(&v).is_err(), "count {bad} was taken");
        }
        for bad in ["01", "1.", ".5", "+1", "0x10", "1e", "-"] {
            assert!(json::parse(bad).is_err(), "{bad} was taken as a number");
        }
        let mut w = Window::new(WindowState::Failed);
        w.reason =
            "mmap \"/Users/zo\u{eb}/Fevom\\data.img\":\n\tno\u{1}memory \u{1f600}".to_string();
        let mut text = String::new();
        w.encode(&mut text);
        assert_eq!(Window::decode(&json::parse(&text).unwrap()).unwrap(), w);
        let v = json::parse(r#""a\/bé😀\b\f""#).unwrap();
        assert_eq!(v, Value::String("a/b\u{e9}\u{1f600}\u{8}\u{c}".to_string()));
        for bad in [
            r#""\ud83d""#,
            r#""\ud83dx""#,
            r#""\ude00""#,
            r#""\x""#,
            r#""\u12""#,
            "\"a\nb\"",
            r#""open"#,
        ] {
            assert!(json::parse(bad).is_err(), "{bad} was taken as a string");
        }
    }

    #[test]
    fn damaged_lines_are_refused_or_read_and_never_panic() {
        let mut seed = 0x9e3779b97f4a7c15u64;
        let mut next = move || {
            seed ^= seed << 13;
            seed ^= seed >> 7;
            seed ^= seed << 17;
            seed
        };
        let mut read = 0;
        for (_, line) in session() {
            for _ in 0..400 {
                let mut bytes = line.clone().into_bytes();
                for _ in 0..1 + next() % 3 {
                    let at = (next() % bytes.len() as u64) as usize;
                    match next() % 4 {
                        0 => bytes[at] = next() as u8,
                        1 => bytes.truncate(at),
                        2 => {
                            let piece = bytes[at..].to_vec();
                            bytes.extend_from_slice(&piece);
                        }
                        _ => bytes[at] = b"{}[]\",:\\0-e."[(next() % 12) as usize],
                    }
                    if bytes.is_empty() {
                        break;
                    }
                }
                match parse(&bytes) {
                    Ok(Message::Request(req)) => {
                        let _ = req.call();
                        read += 1;
                    }
                    Ok(Message::Reply {
                        result: Ok(value), ..
                    }) => {
                        for method in [
                            method::PAUSE,
                            method::RESUME,
                            method::STATS,
                            method::STOP,
                            method::RESET,
                        ] {
                            if let Ok(text) = again(method, &value) {
                                let back = json::parse(&text).expect("what we write is JSON");
                                assert_eq!(again(method, &back).as_deref(), Ok(text.as_str()));
                            }
                        }
                        read += 1;
                    }
                    Ok(Message::Reply { .. }) | Err(_) => {}
                }
            }
        }
        assert!(read > 100, "only {read} damaged lines were still read");
    }
}
