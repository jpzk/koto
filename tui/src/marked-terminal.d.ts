declare module 'marked-terminal' {
  // Minimal shim — marked-terminal ships no types. Whatever object it returns
  // is fed to marked.use() which accepts unknown extensions.
  export function markedTerminal(opts?: Record<string, unknown>): unknown;
}
