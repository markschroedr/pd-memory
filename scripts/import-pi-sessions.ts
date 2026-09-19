// Existing daily jobs use --root. Keep their entry point and Pi capture IDs intact.
import { main } from "./import-sessions";

await main(process.argv.slice(2).map(value => value === "--root" ? "--pi" : value));
