import {defineConfig} from '@playwright/test';
export default defineConfig({
 testDir:'./tests',fullyParallel:false,workers:1,timeout:240_000,
 expect:{timeout:15_000},outputDir:'../../reports/ui/browser',
 use:{baseURL:'http://127.0.0.1:5173',browserName:'chromium',trace:'retain-on-failure',screenshot:'only-on-failure',extraHTTPHeaders:{Origin:'http://127.0.0.1:5173'}},
 webServer:{command:'npm run dev',url:'http://127.0.0.1:5173',timeout:30_000,reuseExistingServer:process.env.GIT_STATE_UI_REUSE_SERVER==='1'},
});
