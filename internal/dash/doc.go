// Package dash renders keep's four read-only dashboard views (queue,
// backlog, item, tree) as HTML via html/template. It exists under two rules,
// each load-bearing enough to write down here rather than leave implicit:
//
//  1. internal/dash carries no behaviour of its own. It calls internal/store
//     and renders. Ordering, grouping, filtering, and blocked-ness are the
//     store's, already computed before a []model.Item or []model.TreeNode
//     ever reaches this package. If a view needs data shaped differently,
//     that shaping belongs in a store method where the CLI reaches it too --
//     the same rule internal/mcp lives under (MCP-05), for the same reason:
//     one place computes the answer, every surface renders it.
//
//  2. No agent-supplied field ever reaches a resource-fetching attribute.
//     href, src, srcset, action, formaction, poster, and style values are
//     built only from .ID (store-generated) or from fixed literal paths in
//     the template source -- never from a title, a body, a why, or any other
//     field an agent can set. html/template will NOT catch a violation of
//     this rule: verified live during 05 planning, an href interpolating a
//     well-formed "http://" or "//" value passes through completely
//     unchanged, and so does a `<style>` block interpolating one via
//     `url(...)`. Contextual auto-escaping is a real defense against
//     quote-breaking and javascript: URIs; it was never designed to answer
//     "does this URL point at this host," which is exactly what DASH-05
//     asks. The guarantee DASH-05 requires is created HERE, by never writing
//     the template action that could violate it -- it is not something
//     html/template hands this package for free.
//
// template.HTML, template.HTMLAttr, template.URL, template.JS, and
// template.CSS are forbidden in this package, with exactly one documented
// exception (templates.go's styleInline). Each of these types tells the
// engine to trust a string literally -- exactly the bypass DASH-05 exists to
// rule out. Plan 06 turns this into a test that must find zero occurrences
// outside that one exception, forever.
package dash
