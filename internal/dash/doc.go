// Package dash renders keep's four read-only dashboard views (queue,
// backlog, item, tree) as HTML via html/template. It follows two rules:
//
//  1. internal/dash carries no behaviour of its own. It calls internal/store
//     and renders. Ordering, grouping, filtering, and blocked-ness are the
//     store's, already computed before a []model.Item or []model.TreeNode
//     ever reaches this package. If a view needs data shaped differently,
//     that shaping belongs in a store method where the CLI reaches it too --
//     the same rule internal/mcp lives under, for the same reason:
//     one place computes the answer, every surface renders it.
//
//  2. No agent-supplied field ever reaches a resource-fetching attribute.
//     href, src, srcset, action, formaction, poster, and style values are
//     built only from .ID (store-generated) or from fixed literal paths in
//     the template source -- never from a title, a body, a why, or any other
//     field an agent can set. html/template will NOT catch a violation of
//     this rule: an href interpolating a
//     well-formed "http://" or "//" value passes through completely
//     unchanged, and so does a `<style>` block interpolating one via
//     `url(...)`. Contextual auto-escaping is a real defense against
//     quote-breaking and javascript: URIs; it was never designed to answer
//     "does this URL point at this host," which is exactly what this rule
//     asks. The guarantee is created HERE, by never writing
//     the template action that could violate it -- it is not something
//     html/template hands this package for free.
//
// template.HTML, template.HTMLAttr, template.URL, template.JS, and
// template.CSS are forbidden in this package, with exactly one documented
// exception (templates.go's styleInline). Each of these types tells the
// engine to trust a string literally -- exactly the bypass this rule exists
// to rule out. structure_test.go must find zero occurrences outside that
// one exception.
package dash
