// node shoot.js — regenerates the PNGs from prototype.html (needs playwright)
const {chromium}=require('/tmp/pw/node_modules/playwright');
(async()=>{const b=await chromium.launch();
for(const [w,h,s,tag] of [[1000,560,1,'desktop'],[390,700,2,'phone']]){
 const c=await b.newContext({viewport:{width:w,height:h},deviceScaleFactor:s});
 for(const v of ['A','B','C'])for(const st of ['idle','ask']){
  const p=await c.newPage();await p.goto(`file://${__dirname}/prototype.html?shot#${v}-${st}`);await p.evaluate(()=>document.fonts.ready);await p.waitForTimeout(300);
  await p.screenshot({path:`variant-${v}-${st}-${tag}.png`});await p.close();}
}await b.close()})();
