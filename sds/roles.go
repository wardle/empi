// Package sds provides preliminary support for the NHS England staff directory (SDS)
//
// Roles.go provides resolution services for the SDS job name.
// See https://fhir.nhs.uk/STU3/CodeSystem/CareConnect-SDSJobRoleName-1
//
package sds

import (
	"context"
	"fmt"
	"strings"

	"github.com/wardle/concierge/apiv1"
	"github.com/wardle/concierge/identifiers"
	snomed "github.com/wardle/go-terminology/snomed"
	"google.golang.org/protobuf/proto"
)

const (
	// SDSJobRoleNameURI represents the URI for this system of identifiers
	SDSJobRoleNameURI = "https://fhir.nhs.uk/STU3/CodeSystem/CareConnect-SDSJobRoleName-1"
)

var codes = make(map[string]*apiv1.Role)
var jobTitles = make(map[string]string)

func init() {
	identifiers.Register("SDS Job Roles", SDSJobRoleNameURI)
	identifiers.RegisterResolver(SDSJobRoleNameURI, roleResolver)
	// split our SDS data into something manageable
	for _, entry := range strings.Split(sdsData, "\n") {
		words := strings.Fields(entry)
		if len(words) == 0 {
			continue
		}
		code := words[0]
		deprecated := false
		if words[len(words)-1] == "(Closed)" {
			words = words[:len(words)-1] // remove last word
			deprecated = true
		}
		jobTitle := strings.Join(words[1:], " ")
		codes[code] = &apiv1.Role{
			JobTitle:   jobTitle,
			Deprecated: deprecated,
		}
		jobTitles[jobTitle] = code
	}
	// build a reverse map
	for sds, sct := range sdsMapping {
		sdsReverseMapping[sct] = sds
	}
	// register our identifier mappers
	identifiers.RegisterMapper(SDSJobRoleNameURI, identifiers.SNOMEDCT, mapSDStoSNOMED)
	identifiers.RegisterMapper(identifiers.SNOMEDCT, SDSJobRoleNameURI, mapSNOMEDtoSDS)
}

// roleResolver provides a resolution service for the SDS role value set
func roleResolver(ctx context.Context, id *apiv1.Identifier) (proto.Message, error) {
	if role, ok := codes[id.Value]; ok {
		return role, nil
	}
	return nil, identifiers.ErrNotFound
}

func mapSDStoSNOMED(ctx context.Context, id *apiv1.Identifier) (*apiv1.Identifier, error) {
	if sctID, found := sdsMapping[id.GetValue()]; found {
		return &apiv1.Identifier{
			System: identifiers.SNOMEDCT,
			Value:  string(sctID),
		}, nil
	}
	return nil, identifiers.ErrNotFound
}

// TODO: should use SNOMED service to automatically check is type of occupation, and then
// find the map.
func mapSNOMEDtoSDS(ctx context.Context, id *apiv1.Identifier) (*apiv1.Identifier, error) {
	sctID, err := snomed.ParseValidIdentifier(id.GetValue(), true)
	if err != nil {
		return nil, fmt.Errorf("cannot map from SNOMED '%s': %w", id.GetValue(), err)
	}
	if !sctID.IsConcept() {
		return nil, fmt.Errorf("cannot map from SNOMED, expected concept, got: %s", sctID)
	}
	if sds, found := sdsReverseMapping[uint64(sctID)]; found {
		return &apiv1.Identifier{
			System: SDSJobRoleNameURI,
			Value:  sds,
		}, nil
	}
	return nil, identifiers.ErrNotFound
}

var sdsReverseMapping = map[uint64]string{}

// SNOMED SDS mapping - incomplete TODO: complete - probably semi-automatically if possible
var sdsMapping = map[string]uint64{
	"R0050": 768839008,
	"R0030": 158890004,
	"R0040": 768839008, // senior lecturer doesn't exist, so we use 'consultant'
	"R0070": 309396002,
	"R0080": 397908005,
	"R0100": 224529009,
	"R0110": 302211009,
	"R0120": 224530004,
	"R0130": 224531000,
	"R0140": 224532007,
	"R0150": 158972004,
	"R0260": 62247001,
	"R0370": 309454000,
	"R0790": 159033005,
	"R0018": 309418004,
	"R1760": 394572006,
}

// This list was copy and pasted from
// https://fhir.nhs.uk/STU3/CodeSystem/CareConnect-SDSJobRoleName-1
// on 15/3/2020
var sdsData = `R0010	Medical Director
R0020	Clinical Director - Medical
R0210	Director of Public Health
R0030	Professor
R0040	Senior Lecturer
R0050	Consultant
R0060	Special Salary Scale in Public Health Medicine
R0070	Associate Specialist
R0080	Staff Grade
R0090	Hospital Practitioner
R0100	Clinical Assistant
R0110	Specialist Registrar
R0120	Senior Registrar (Closed)
R0130	Registrar (Closed)
R0140	Senior House Officer
R0150	House Officer - Pre Registration
R0160	House Officer - Post Registration
R0170	Trust Grade Doctor - House Officer level
R0180	Trust Grade Doctor - SHO level
R0190	Trust Grade Doctor - Specialist Registrar level
R0200	Trust Grade Doctor - Career Grade level
R0260	General Medical Practitioner
R0270	Salaried General Practitioner
R1981	Psychiatrist
R1984	Health Records Administrator
R6200	GP Registrar
R6300	Sessional GP
R7140	ODP
R7150	SODP
R9100	A&E Staff Nurse (Temporary) London Cluster Only
R9101	A&E Manager (Temporary) London Cluster Only
R9102	A&E Doctor (Temporary) London Cluster only
R9103	A&E Student (Temporary) London Cluster Only
R9104	A&E Clerk (Temporary) London Cluster Only
R0215	Assistant Clinical Medical Officer
R0220	Clinical Medical Officer
R0230	Senior Clinical Medical Officer
R0240	Other Community Health Service
R0243	Other Community Health Service - Social Care Worker
R0247	Other Community Health Service - Admin Clerk
R0055	Dental Surgeon acting as Hospital Consultant
R0250	General Dental Practitioner
R0280	Regional Dental Officer
R0290	Dental Clinical Director - Dental
R0295	Dental Assistant Clinical Director
R0300	Dental Officer
R0310	Senior Dental Officer
R0320	Salaried Dental Practitioner
R0006	Student Community Practitioner
R0330	Student Nurse - Adult Branch
R0340	Student Nurse - Child Branch
R0350	Student Nurse - Mental Health Branch
R0360	Student Nurse - Learning Disabilities Branch
R0370	Student Midwife
R0380	Student Health Visitor
R0390	Student District Nurse
R0400	Student School Nurse
R0410	Student Practice Nurse
R0420	Student Occupational Health Nurse
R0430	Student Community Paediatric Nurse
R0440	Student Community Mental Health Nurse
R0450	Student Community Learning Disabilities Nurse
R0460	Student Chiropodist
R0470	Student Dietitian
R0480	Student Occupational Therapist
R0490	Student Orthoptist
R0500	Student Physiotherapist
R0510	Student Radiographer - Diagnostic
R0520	Student Radiographer - Therapeutic
R0530	Student Speech & Language Therapist
R0540	Art, Music & Drama Student
R0550	Student Psychotherapist
R6400	Medical Student
R0560	Director of Nursing
R0580	Nurse Manager
R0610	Sister/Charge Nurse
R1976	Community Team Manager
R0570	Nurse Consultant
R0600	Specialist Nurse Practitioner
R0620	Staff Nurse
R0630	Enrolled Nurse
R0690	Community Practitioner
R0700	Community Nurse
R1974	Community Learning Disabilities Nurse
R1975	Community Mental Health Nurse
R0590	Modern Matron
R1972	Clinical Team Manager
R0640	Midwife - Consultant
R0650	Midwife - Specialist Practitioner
R0660	Midwife - Manager
R0670	Midwife - Sister/Charge Nurse
R0680	Midwife
R0018	Audiologist
R0750	Chiropodist/Podiatrist
R0760	Chiropodist/Podiatrist Consultant
R0770	Chiropodist/Podiatrist Manager
R0780	Chiropodist/Podiatrist Specialist Practitioner
R0790	Dietitian
R0800	Dietitian Consultant
R0810	Dietitian Manager
R0820	Dietitian Specialist Practitioner
R0950	Occupational Therapist
R0960	Occupational Therapist Consultant
R0970	Occupational Therapist Manager
R0980	Occupational Therapy Specialist Practitioner
R0990	Orthoptist
R1000	Orthoptist Consultant
R1010	Orthoptist Manager
R1020	Orthoptist Specialist Practitioner
R1110	Physiotherapist
R1120	Physiotherapist Consultant
R1130	Physiotherapist Manager
R1140	Physiotherapist Specialist Practitioner
R1070	Paramedic
R1080	Paramedic Consultant
R1090	Paramedic Manager
R1100	Paramedic Specialist Practitioner
R0003	Clinical Application Administrator
R0012	Radiographer
R0013	Student Radiographer
R0014	Radiologist
R0015	PACS Administrator
R0016	Reporting Radiographer
R0017	Assistant Practitioner
R1190	Radiographer - Diagnostic
R1200	Radiographer - Diagnostic, Consultant
R1210	Radiographer - Diagnostic, Manager
R1220	Radiographer - Diagnostic, Specialist Practitioner
R1230	Radiographer - Therapeutic
R1240	Radiographer - Therapeutic, Consultant
R1250	Radiographer - Therapeutic, Manager
R1260	Radiographer - Therapeutic, Specialist Practitioner
R1030	Orthotist
R1040	Orthotist Consultant
R1050	Orthotist Manager
R1060	Orthotist Specialist Practitioner
R1150	Prosthetist
R1160	Prosthetist Consultant
R1170	Prosthetist Manager
R1180	Prosthetist Specialist Practitioner
R0710	Art Therapist
R0720	Art Therapist Consultant
R0730	Art Therapist Manager
R0740	Art Therapist Specialist Practitioner
R0830	Drama Therapist
R0840	Drama Therapist Consultant
R0850	Drama Therapist Manager
R0860	Drama Therapist Specialist Practitioner
R0870	Multi Therapist
R0880	Multi Therapist Consultant
R0890	Multi Therapist Manager
R0900	Multi Therapist Specialist Practitioner
R0910	Music Therapist
R0920	Music Therapist Consultant
R0930	Music Therapist Manager
R0940	Music Therapist Specialist Practitioner
R0955	Speech & Language Therapist
R0965	Speech & Language Therapist Consultant
R0975	Speech & Language Therapist Manager
R0985	Speech & Language Therapist Specialist Practitioner
R9500	Social Services Senior Management
R9505	Social Services Policy and Planning
R9510	Social Services Information Manager
R9515	Social Work Team Manager (Children)
R9520	Senior Social Worker (Children)
R9525	Social Services Care Manager (Children)
R9530	Social Work Assistant (Children)
R9535	Child Protection Worker
R9540	Family Placement Worker
R9545	Community Worker (Children)
R9550	Occupational Therapist
R9555	Occupational Therapist Assistant
R9560	Occupational Therapy Team Manager
R9565	Social Work Team Manager (Adults)
R9570	Senior Social Worker (Adults)
R9575	Social Services Care Manager (Adults)
R9580	Social Work Assistant (Adults)
R9585	Social Work Team Manager (Mental Health)
R9590	Senior Social Worker (Mental Health)
R9595	Social Services Care Manager (Mental Health)
R9600	Social Work Assistant (Mental Health)
R9605	Emergency Duty Social Worker
R9615	Social Services Driver
R9620	Home Care Organiser
R9625	Home Care Administrator
R9630	Home Help
R9635	Warden
R9640	Meals on Wheels Organiser
R9645	Meals Delivery
R9650	Day Centre Manager
R9655	Day Centre Deputy
R9660	Day Centre Officer
R9665	Day Centre Care Staff
R9670	Family Centre Manager
R9675	Family Centre Deputy
R9680	Family Centre Worker
R9685	Nursery Manager
R9690	Nursery Deputy
R9695	Nursery Worker
R9700	Playgroup Leader
R9705	Playgroup Assistant
R9710	Residential Manager
R9715	Residential Deputy
R9720	Residential Worker
R9725	Residential Care Staff
R9730	Intermediate Care Manager
R9735	Intermediate Care Deputy
R9740	Intermediate Care Worker
R9745	Intermediate Care Staff
R9750	Social Care SAP User
R9755	Social Care SAP Manager
R1270	Clinical Director
R1280	Optometrist
R1290	Pharmacist
R1979	Medical Technical Officer - Pharmacy
R1300	Psychotherapist
R1310	Clinical Psychologist
R1320	Chaplain
R1330	Social Worker
R1340	Approved Social Worker
R1350	Youth Worker
R1360	Specialist Practitioner
R1370	Practitioner
R0011	Dispenser
R1380	Technician - PS&T
R1390	Osteopath
R1400	Healthcare Scientist
R1410	Consultant Healthcare Scientist
R1420	Biomedical Scientist
R0019	Medical Technical Officer
R1430	Technician - Healthcare Scientists
R1440	Therapist
R1540	Associate Practitioner
R1543	Associate Practitioner - Nurse
R1547	Associate Practitioner - General Practitioner
R1560	Helper/Assistant
R1600	Cytoscreener
R1570	Dental Surgery Assistant
R1450	Health Care Support Worker
R1580	Medical Laboratory Assistant
R1550	Counsellor
R0002	Porter
R1690	Call Operator
R1700	Gateway Worker
R1710	Support, Time, Recovery Worker
R1480	Healthcare Assistant
R1490	Nursery Nurse
R1590	Phlebotomist
R1460	Social Care Support Worker
R1470	Home Help
R1520	Technician - Additional Clinical Services
R1530	Technical Instructor
R1980	Patient Welfare Officer
R1500	Play Therapist
R1510	Play Specialist
R1610	Student Technician
R1620	Trainee Scientist
R1630	Trainee Practitioner
R1640	Nursing Cadet
R1650	Healthcare Cadet
R1660	Pre-reg Pharmacist
R1670	Assistant Psychologist
R1680	Assistant Psychotherapist
R0007	ERS SDS Organisation Reporting Analyst
R0008	Demographic Supervisor
R0021	DSA NHS Number Manager (Temporary)
R0022	DSA National Clinical Supervisor (Temporary)
R0023	DSA National Clinical Administrator (Temporary)
R1720	Clerical Worker
R1730	Receptionist
R1740	Secretary
R1750	Personal Assistant
R1751	Demographic Administrator (Sensitive Records) Temporary
R1760	Medical Secretary
R1770	Officer
R1971	Map of Medicine Administrator
R1973	Community Administrator
R1977	ECC/CPA Administrator
R1978	Information Officer
R1985	Health Records Clerk
R1995	End Point Approver
R5010	Network Technician
R5040	Desktop Support Administrator
R5090	Registration Authority Agent
R5110	Demographic Administrator
R5120	ISP Administrator
R5130	Technical Codes Administrator
R5140	OSS Administrator
R5170	End Point Administrator
R5175	End Point Viewer
R5181	RTS Dashboard User
R5183	RTS BT Dashboard User
R5186	ERS BT Customer SLA User
R5188	ERS BT Supplier SLA User
R5189	ERS LogicaCMG SLA User
R5190	Content Creator
R5195	Content Publisher
R5210	User Details Administrator
R5250	EBS Administrator
R6010	Appointments Clerk
R6030	Ward Clerk
R6050	Clinical Coder
R6060	Medical Records Clerk
R6080	Waiting List Clerk
R7100	Trainer
R7110	Training Manager
R7120	Directory of Services Coordinator
R9756	ETP System Administrator
R1780	Manager
R1790	Senior Manager
R1910	Chair
R1920	Chief Executive
R1930	Finance Director
R1940	Other Executive Director
R1950	Board Level Director
R1960	Non Executive Director
R1970	Childcare Co-ordinator
R1982	Senior Administrator
R1983	Ward Manager
R1986	Workgroup Administrator
R1987	National RBAC Attribute Administrator
R1988	National RBAC Baseline Policy Administrator
R1989	Complaints Coordinator
R1990	Complaints Investigator
R1996	End Point DNS Administrator
R1997	End Point Spine Administrator
R1998	End Point Super User
R1999	End Point Service Administrator
R5000	Network Administrator
R5003	Cluster System Administrator
R5007	System Administrator
R5020	Helpdesk Administrator
R5060	Security Policy Controller
R5070	Senior Security Manager
R5072	Root Registration Authority Manager
R5080	Registration Authority Manager
R5100	Audit Manager
R5105	Caldicott Guardian
R5180	NASP Service Manager
R5182	ERS ETP System Administrator
R5184	ERS Spine SLA Manager
R5185	ERS BT Customer SLA Manager
R5187	ERS BT Supplier SLA Manager
R5191	ERS Support Administrator
R5192	ECS Administrator
R5300	Portal Administrator
R5310	LiquidLogic Administrator
R5320	i.EPR Administrator
R5330	Synergy Administrator
R5340	SystmOne Administrator
R6020	Outpatient Manager
R6040	Bed Manager
R6070	Medical Records Manager
R6090	Waiting List Manager
R6100	Mental Health Act Administrator
R6160	Ad-hoc Report Manager
R7130	PAS Manager
R1800	Technician - Admin & Clerical
R1810	Accountant
R1820	Librarian
R1830	Interpreter
R1840	Analyst
R1850	Adviser
R1860	Researcher
R1870	Control Assistant
R1880	Architect
R1890	Lawyer
R1900	Surveyor
R5030	Helpdesk Technician
R5050	Desktop Support Technician
R5150	System Worker
R5400	Availability Monitor
R8000	Clinical Practitioner Access Role
R8001	Nurse Access Role
R8002	Nurse Manager Access Role
R8003	Health Professional Access Role
R8004	Healthcare Student Access Role
R8016	Midwife Access Role
R8017	Midwife Manager Access Role
R8024	Bank Access Role
R8005	Biomedical Scientist Access Role
R8006	Medical Secretary Access Role
R8007	Clinical Coder Access Role
R8008	Admin/Clinical Support Access Role
R8015	Systems Support Access Role
R0001	Privacy Officer
R8009	Receptionist Access Role
R8010	Clerical Access Role
R8011	Clerical Manager Access Role
R8012	Information Officer Access Role
R8013	Health Records Manager Access Role
R8014	Social Worker Access Role`
