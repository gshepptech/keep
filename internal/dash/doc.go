// Package dash renders keep's four read-only dashboard views (queue,
// backlog, item, tree) as HTML via html/template. It follows two rules:
//
//  1. internal/dash carries no behaviour of its own. It calls internal/store
//     and renders. Ordering, grouping, filtering, and blocked-ness are the
//     store's, already computed before a []model.Item or []model.TreeNode
//     ever reaches this package. If a view needs data shaped differently,
//     that shaping belongs in a store method where the CLI reaches it too.
//     internal/mcp follows the same rule for the same reason: one place
//     computes the answer, every surface renders it.
//
//  2. No agent-supplied field ever reaches a resource-fetching attribute.
//     href, src, srcset, action, formaction, poster, and style values are
//     built only from .ID (store-generated) or from fixed literal paths in
//     the template source, never from a title, a body, a why, or any other
//     field an agent can set. html/template does not catch a violation of
//     this rule: an href interpolating a well-formed "http://" or "//"
//     value passes through unchanged, and so does a `<style>` block
//     interpolating one via `url(...)`. Contextual auto-escaping defends
//     against quote-breaking and javascript: URIs; it does not answer "does
//     this URL point at this host," which is what this rule asks. The
//     guarantee comes from never writing the template action that could
//     violate it, not from html/template.
//
// template.HTML, template.HTMLAttr, template.URL, template.JS, and
// template.CSS are forbidden in this package, with exactly one documented
// exception (templates.go's styleInline). Each of these types tells the
// engine to trust a string literally, which is the bypass this rule exists
// to rule out. structure_test.go fails on any occurrence outside that one
// exception.
package dash
