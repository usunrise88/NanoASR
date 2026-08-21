/**
 * Every route file must export `pageMeta`.
 *
 * The root shell renders titles, spacing and breadcrumbs from typed route
 * context (SPEC §13.2). A route that does not declare its metadata would either
 * render headerless or be tempted to draw its own header, which is exactly the
 * drift this rule exists to prevent.
 */
export const requirePageMeta = {
  meta: {
    type: 'problem',
    docs: { description: 'route files must export pageMeta consumed by the root layout' },
    schema: [],
    messages: {
      missing:
        'Route file must `export const pageMeta: PageMeta = {…}`. The root shell renders the ' +
        'page header from it; do not render a header inside the route.',
    },
  },
  create(context) {
    const filename = context.filename ?? context.getFilename()
    if (!/\/src\/routes\/.*\.tsx$/.test(filename)) return {}
    if (/__root\.tsx$/.test(filename)) return {}

    let found = false
    return {
      ExportNamedDeclaration(node) {
        const decl = node.declaration
        if (decl?.type !== 'VariableDeclaration') return
        for (const d of decl.declarations) {
          if (d.id?.type === 'Identifier' && d.id.name === 'pageMeta') found = true
        }
      },
      'Program:exit'(node) {
        if (!found) context.report({ node, messageId: 'missing' })
      },
    }
  },
}

export default { rules: { 'require-page-meta': requirePageMeta } }
