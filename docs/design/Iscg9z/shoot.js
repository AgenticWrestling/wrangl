// node shoot.js — regenerates PNGs from prototype.html (needs playwright at /tmp/pw)
const {chromium}=require('/tmp/pw/node_modules/playwright');
(async()=>{const b=await chromium.launch();
 for(const [w,h,sc,tag] of [[900,900,1,'desktop'],[390,844,2,'phone']]){
  const ctx=await b.newContext({viewport:{width:w,height:h},deviceScaleFactor:sc});
  for(const v of ['A','B','C']){
   const shots={
    collapsed:{},
    filtered:{q:'bug',types:[]},
    types:{types:['ux','bug'],menu:v!='B'},
    done:{done:true,types:[]},
    empty:{q:'zzz'},
   };
   for(const [st,o] of Object.entries(shots)){
    const p=await ctx.newPage();
    for(let i=0;i<10;i++){await p.goto(`file://${__dirname}/prototype.html?shot#${v}`);
     const ok=await p.evaluate(async()=>{try{await document.fonts.load('400 16px "Red Hat Display"');await document.fonts.load('14px "IBM Plex Mono"');return true}catch(e){return false}});if(ok)break}
    await p.evaluate(o=>demo.set(Object.assign({q:'',types:[],done:false,menu:false,open:{}},o)),o);
    await p.waitForTimeout(200);
    await p.locator('#card').screenshot({path:`variant-${v}-${st}-${tag}.png`});await p.close();}}
 }await b.close()})();
