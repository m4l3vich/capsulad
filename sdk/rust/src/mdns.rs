//! mDNS publishing and browsing through the daemon's Avahi responder.
//!
//! Requests and responses are canonical protobuf JSON for
//! `capsulad.framework.v1.DiscoveryService`; field names pass through
//! unchanged. A browse is a server-streaming call exposed as a synchronous
//! iterator: [`Browse::next`] returns `None` at a clean end of stream.

use std::collections::BTreeMap;
use std::time::Duration;

use serde_json::{Map, Value};

use crate::call::Call;
use crate::client::parse_json;
use crate::error::{self, Error, Result};
use crate::net::{number_field, string_field};
use crate::sys::{self, Buf};
use crate::{status_error, take_buf, timeout_ms};

/// A service instance to publish (`PublishServiceRequest`).
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct MdnsService {
    /// `"_snapcast._tcp"`, `"_googlecast._tcp"`, `"_airplay._tcp"`, ...
    pub service_type: String,
    /// Displayed instance name, at most 63 UTF-8 bytes.
    pub instance_name: String,
    /// Advertised port, 1..65535.
    pub port: u32,
    /// At most 16 records totalling at most 1024 bytes.
    pub txt: BTreeMap<String, String>,
    /// `None` or empty means `"local"`.
    pub domain: Option<String>,
}

impl MdnsService {
    fn encode(&self) -> Value {
        let mut map = Map::new();
        map.insert(
            "serviceType".into(),
            Value::String(self.service_type.clone()),
        );
        map.insert(
            "instanceName".into(),
            Value::String(self.instance_name.clone()),
        );
        map.insert("port".into(), Value::from(self.port));
        if !self.txt.is_empty() {
            let txt: Map<String, Value> = self
                .txt
                .iter()
                .map(|(key, value)| (key.clone(), Value::String(value.clone())))
                .collect();
            map.insert("txt".into(), Value::Object(txt));
        }
        if let Some(domain) = &self.domain {
            if !domain.is_empty() {
                map.insert("domain".into(), Value::String(domain.clone()));
            }
        }
        Value::Object(map)
    }
}

/// A successful publication (`PublishServiceResponse`).
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct MdnsPublication {
    pub publication_id: String,
    pub service_type: String,
    pub instance_name: String,
    pub domain: String,
    pub host: String,
    pub port: u32,
}

/// One `DiscoveredService` browse event. `kind` is the canonical enum name
/// (`EVENT_KIND_ADDED` / `EVENT_KIND_REMOVED`).
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct DiscoveredService {
    pub kind: String,
    pub name: String,
    pub service_type: String,
    pub domain: String,
    pub host: String,
    pub addresses: Vec<String>,
    pub port: u32,
    pub txt: BTreeMap<String, String>,
}

impl DiscoveredService {
    fn from_value(value: &Value) -> Self {
        let addresses = value
            .get("addresses")
            .and_then(Value::as_array)
            .map(|list| {
                list.iter()
                    .filter_map(Value::as_str)
                    .map(str::to_string)
                    .collect()
            })
            .unwrap_or_default();
        let txt = value
            .get("txt")
            .and_then(Value::as_object)
            .map(|map| {
                map.iter()
                    .filter_map(|(key, value)| {
                        value.as_str().map(|text| (key.clone(), text.to_string()))
                    })
                    .collect()
            })
            .unwrap_or_default();
        Self {
            kind: string_field(value, "kind"),
            name: string_field(value, "name"),
            service_type: string_field(value, "serviceType"),
            domain: string_field(value, "domain"),
            host: string_field(value, "host"),
            addresses,
            port: number_field(value, "port") as u32,
            txt,
        }
    }
}

/// Server-streaming Browse iterator. Dropping it cancels and releases the
/// native call.
pub struct Browse {
    call: Call,
}

impl Browse {
    /// Waits for one event. `None` is a clean end of stream; `None` for
    /// `timeout` waits forever, while a `TIMEOUT` error leaves the stream
    /// usable.
    pub fn next(&self, timeout: Option<Duration>) -> Result<Option<DiscoveredService>> {
        match self.call.recv_json(timeout)? {
            None => Ok(None),
            Some(bytes) => {
                let value = parse_json(&bytes, "mdns event")?;
                Ok(Some(DiscoveredService::from_value(&value)))
            }
        }
    }

    /// Cancels the browse call; pending and later `next` calls fail with
    /// `CANCELLED`.
    pub fn cancel(&self) -> Result<()> {
        self.call.cancel()
    }
}

impl crate::Client {
    /// Publishes one service instance. Requires a connected supervised app
    /// client; a runtime without Avahi reports `UNSUPPORTED`.
    pub fn publish_mdns(
        &self,
        service: &MdnsService,
        timeout: Option<Duration>,
    ) -> Result<MdnsPublication> {
        let request = service.encode().to_string();
        let mut out = Buf::default();
        let status = unsafe {
            (self.inner.api.mdns_publish)(
                self.handle(),
                request.as_ptr(),
                request.len(),
                timeout_ms(timeout),
                &mut out,
            )
        };
        if status != sys::STATUS_OK {
            let err = status_error(&self.inner, status, "mdns_publish");
            unsafe { take_buf(&self.inner, out) };
            return Err(err);
        }
        let bytes = unsafe { take_buf(&self.inner, out) };
        let value = parse_json(&bytes, "mdns publication")?;
        Ok(MdnsPublication {
            publication_id: string_field(&value, "publicationId"),
            service_type: string_field(&value, "serviceType"),
            instance_name: string_field(&value, "instanceName"),
            domain: string_field(&value, "domain"),
            host: string_field(&value, "host"),
            port: number_field(&value, "port") as u32,
        })
    }

    /// Withdraws a publication by id. Publications are also removed when the
    /// run exits.
    pub fn unpublish_mdns(&self, publication_id: &str, timeout: Option<Duration>) -> Result<()> {
        if publication_id.is_empty() {
            return Err(Error::local(
                error::CODE_INVALID_ARGUMENT,
                "publication_id is required",
            ));
        }
        let status = unsafe {
            (self.inner.api.mdns_unpublish)(
                self.handle(),
                publication_id.as_ptr().cast(),
                publication_id.len(),
                timeout_ms(timeout),
            )
        };
        crate::check_status(&self.inner, status, "mdns_unpublish")
    }

    /// Starts the server-streaming Browse call. Cancelling/dropping the
    /// returned [`Browse`] releases the native call.
    pub fn browse_mdns(
        &self,
        service_type: &str,
        domain: Option<&str>,
        timeout: Option<Duration>,
    ) -> Result<Browse> {
        let mut request = Map::new();
        request.insert(
            "serviceType".into(),
            Value::String(service_type.to_string()),
        );
        if let Some(domain) = domain {
            if !domain.is_empty() {
                request.insert("domain".into(), Value::String(domain.to_string()));
            }
        }
        let request = Value::Object(request).to_string();
        let mut handle = 0u64;
        let status = unsafe {
            (self.inner.api.mdns_browse_open)(
                self.handle(),
                request.as_ptr(),
                request.len(),
                timeout_ms(timeout),
                &mut handle,
            )
        };
        crate::check_status(&self.inner, status, "mdns_browse_open")?;
        if handle == 0 {
            return Err(Error::local(
                sys::STATUS_INTERNAL,
                "mdns_browse_open returned a zero handle",
            ));
        }
        Ok(Browse {
            call: Call::from_handle(self.inner.clone(), handle),
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn publish_request_uses_canonical_field_names() {
        let mut txt = BTreeMap::new();
        txt.insert("version".to_string(), "1".to_string());
        let value = MdnsService {
            service_type: "_snapcast._tcp".into(),
            instance_name: "Living Room".into(),
            port: 1704,
            txt,
            domain: None,
        }
        .encode();
        assert_eq!(value["serviceType"], "_snapcast._tcp");
        assert_eq!(value["instanceName"], "Living Room");
        assert_eq!(value["port"], 1704);
        assert_eq!(value["txt"]["version"], "1");
        assert!(value.get("domain").is_none());
    }

    #[test]
    fn discovered_service_decodes_canonical_json() {
        let value: Value = serde_json::from_str(
            r#"{"kind":"EVENT_KIND_ADDED","name":"Kitchen","serviceType":"_airplay._tcp",
                "domain":"local","host":"kitchen.local","addresses":["192.0.2.10"],
                "port":7000,"txt":{"model":"x"}}"#,
        )
        .expect("json");
        let event = DiscoveredService::from_value(&value);
        assert_eq!(event.kind, "EVENT_KIND_ADDED");
        assert_eq!(event.addresses, vec!["192.0.2.10".to_string()]);
        assert_eq!(event.port, 7000);
        assert_eq!(event.txt["model"], "x");
    }
}
