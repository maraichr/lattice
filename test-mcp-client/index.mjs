import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { SSEClientTransport } from "@modelcontextprotocol/sdk/client/sse.js";

const transport = new SSEClientTransport(new URL("http://localhost:8090/sse"));
const client = new Client(
  { name: "test-client", version: "1.0.0" },
  { capabilities: {} }
);

async function main() {
  console.log("Connecting to MCP server via SSE at http://localhost:8090/mcp...");
  await client.connect(transport);
  console.log("Connected successfully!\n");
  
  // List tools
  const tools = await client.listTools();
  console.log("Available Tools:");
  tools.tools.forEach(t => console.log(` - ${t.name}: ${t.description.split('.')[0]}`));
  
  // Call list_projects
  console.log("\nCalling 'list_projects'...");
  const projectsRes = await client.callTool({
    name: "list_projects",
    arguments: {}
  });
  
  const projectsText = projectsRes.content[0].text;
  console.log(projectsText);

  // Extract the project slug from the output
  const match = projectsText.match(/\- \*\*([^*]+)\*\*/);
  if (match) {
      const projectSlug = match[1];
      console.log(`\nCalling 'ask_codebase' on project '${projectSlug}'...`);
      const result = await client.callTool({
          name: "ask_codebase",
          arguments: {
              project: projectSlug,
              query: "What is this application about?"
          }
      });
      console.log(result.content[0].text);
  } else {
      console.log("No indexed projects found. Please ensure your project is fully indexed.");
  }

  process.exit(0);
}

main().catch(console.error);
