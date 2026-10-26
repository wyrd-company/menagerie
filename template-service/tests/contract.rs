//! The socket API specification names every route and error kind the service uses.
use menagerie_template_service::server::ROUTES;
use std::collections::BTreeSet;

const SPECIFICATION: &str = concat!(
    env!("CARGO_MANIFEST_DIR"),
    "/../docs/specifications/template-service-api.yml"
);

/// Dashed string literals in the source that are not error kinds. The
/// `hook-*`, `invalid-hook` and `runner-image-unavailable` literals are the
/// workspace service's hook runner refusals, which the service reads.
const NOT_KINDS: [&str; 15] = [
    "candidate-folder",
    "hook-cancelled",
    "hook-container-failed",
    "hook-runner-socket",
    "hook-token-refused",
    "invalid-hook",
    "kebab-case",
    "lower-case",
    "not-previewed",
    "runner-image",
    "runner-image-unavailable",
    "session-key",
    "template-service",
    "workspace-root",
    "workspace-service",
];

fn description() -> String {
    let text = std::fs::read_to_string(SPECIFICATION).unwrap();
    let document: serde_norway::Value = serde_norway::from_str(&text).unwrap();
    document["description"].as_str().unwrap().to_owned()
}

/// Every `"word-word"` literal outside tests in `src/`.
fn source_kinds() -> BTreeSet<String> {
    let mut kinds = BTreeSet::new();
    for entry in std::fs::read_dir(concat!(env!("CARGO_MANIFEST_DIR"), "/src")).unwrap() {
        let text = std::fs::read_to_string(entry.unwrap().path()).unwrap();
        let code = text.split("#[cfg(test)]").next().unwrap();
        for literal in code.split('"').skip(1).step_by(2) {
            let dashed = literal.contains('-')
                && literal
                    .split('-')
                    .all(|p| !p.is_empty() && p.bytes().all(|b| b.is_ascii_lowercase()));
            if dashed && !NOT_KINDS.contains(&literal) {
                kinds.insert(literal.to_owned());
            }
        }
    }
    kinds
}

/// Every `` `kind` (NNN) `` entry of the specification.
fn specified_kinds(description: &str) -> BTreeSet<String> {
    description
        .split('`')
        .collect::<Vec<_>>()
        .windows(2)
        .filter(|w| {
            let after = w[1].trim_start();
            after.starts_with('(')
                && after[1..].chars().take(3).all(|c| c.is_ascii_digit())
                && after[4..].starts_with(')')
        })
        .map(|w| w[0].to_owned())
        .collect()
}

#[test]
fn the_specification_names_every_route() {
    let description = description();
    for route in ROUTES {
        assert!(
            description.contains(&format!("`{route}`")),
            "{route} is not specified"
        );
    }
}

#[test]
fn the_specification_lists_exactly_the_error_kinds_in_use() {
    let specified = specified_kinds(&description());
    let used = source_kinds();
    assert!(used.len() > 40, "source scan found too few kinds: {used:?}");
    let unlisted: Vec<_> = used.difference(&specified).collect();
    let unused: Vec<_> = specified.difference(&used).collect();
    assert!(
        unlisted.is_empty(),
        "kinds missing from the specification: {unlisted:?}"
    );
    assert!(
        unused.is_empty(),
        "specified kinds the service never uses: {unused:?}"
    );
}

#[test]
fn the_kind_reader_sees_a_listed_kind() {
    let kinds = specified_kinds("as `sample-kind` (409), and `other` text (not a status)");
    assert_eq!(kinds, BTreeSet::from(["sample-kind".to_owned()]));
}
